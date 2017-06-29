/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azure

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"sync"

	"strconv"
	"strings"
	"sync/atomic"
	"time"

	storage "github.com/Azure/azure-sdk-for-go/arm/storage"
	azstorage "github.com/Azure/azure-sdk-for-go/storage"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/golang/glog"
	"github.com/rubiojr/go-vhd/vhd"
	kwait "k8s.io/apimachinery/pkg/util/wait"
)

type storageAccountState struct {
	name                    string
	saType                  storage.SkuName
	key                     string
	diskCount               int32
	isValidating            int32
	defaultContainerCreated bool
}

//BlobDiskController : blob disk controller struct
type BlobDiskController struct {
	common   *controllerCommon
	accounts map[string]*storageAccountState
}

var defaultContainerName = ""
var storageAccountNamePrefix = ""
var storageAccountNameMatch = ""
var initFlag int64

var accountsLock = &sync.Mutex{}

func newBlobDiskController(common *controllerCommon) (*BlobDiskController, error) {
	c := BlobDiskController{common: common}
	err := c.init()

	if err != nil {
		return nil, err
	}

	return &c, nil
}

//AttachBlobDisk : attaches a disk to node and return lun # as string
func (c *BlobDiskController) AttachBlobDisk(nodeName string, diskURI string, cacheMode string) (int, error) {
	// K8s in case of existing pods evication, will automatically attepmt to attach volumes
	// to a different node. Though it *knows* which disk attached to which node.
	// the following guards against this behaviour

	// avoid:
	// Azure in case of blob disks, does not maintain a list of vhd:attached-to:node
	// The call  attach-to will fail after it was OK on the ARM VM endpoint
	// possibly putting the entire VM in *failed* state
	noLease, e := c.diskHasNoLease(diskURI)
	if e != nil {
		return -1, e
	}

	if !noLease {
		return -1, fmt.Errorf("azureDisk - disk %s still have leases on it. Will not be able to attach to node %s", diskURI, nodeName)
	}

	var vmData interface{}
	_, diskName, err := diskNameandSANameFromURI(diskURI)
	if err != nil {
		return -1, err
	}

	vm, err := c.common.getArmVM(nodeName)
	if err != nil {
		return 0, err
	}

	if err := json.Unmarshal(vm, &vmData); err != nil {
		return -1, err
	}

	fragment, ok := vmData.(map[string]interface{})
	if !ok {
		return -1, fmt.Errorf("convert vmData to map error")
	}
	// remove "resources" as ARM does not support PUT with "resources"
	delete(fragment, "resources")

	dataDisks, storageProfile, hardwareProfile, err := ExtractVMData(fragment)
	if err != nil {
		return -1, err
	}
	vmSize := hardwareProfile["vmSize"].(string)

	managedVM := c.common.isManagedArmVM(storageProfile)
	if managedVM {
		return -1, fmt.Errorf("azureDisk - error: attempt to attach blob disk %s to an managed node  %s ", diskName, nodeName)
	}

	// lock for findEmptyLun and append disk
	var mutex = &sync.Mutex{}
	mutex.Lock()
	defer mutex.Unlock()

	lun, err := findEmptyLun(vmSize, dataDisks)

	if err != nil {
		return -1, err
	}

	newDisk := &armVMDataDisk{
		Name:         diskName,
		Caching:      cacheMode,
		CreateOption: "Attach",
		//DiskSizeGB:   sizeGB,
		Vhd: &armVMVhdDiskInfo{URI: diskURI},
		Lun: lun,
	}

	dataDisks = append(dataDisks, newDisk)
	storageProfile["dataDisks"] = dataDisks // -> store back

	payload := new(bytes.Buffer)
	err = json.NewEncoder(payload).Encode(fragment)

	if err != nil {
		return -1, err
	}

	err = c.common.updateArmVM(nodeName, payload)
	if err != nil {
		return -1, err
	}

	// We don't need to poll ARM here, since WaitForAttach (running on node) will
	// be looping on the node to get devicepath /dev/sd* by lun#
	glog.V(2).Infof("azureDisk - Attached disk %s to node %s", diskName, nodeName)
	return lun, nil
}

//DetachBlobDisk : detaches disk from a node
func (c *BlobDiskController) DetachBlobDisk(nodeName string, hasheddiskURI string) error {
	diskURI := ""
	var vmData interface{}
	vm, err := c.common.getArmVM(nodeName)

	if err != nil {
		return err
	}

	if err := json.Unmarshal(vm, &vmData); err != nil {
		return err
	}

	fragment, ok := vmData.(map[string]interface{})
	if !ok {
		return fmt.Errorf("convert vmData to map error")
	}
	// remove "resources" as ARM does not support PUT with "resources"
	delete(fragment, "resources")
	dataDisks, storageProfile, _, err := ExtractVMData(fragment)
	if err != nil {
		return err
	}

	// we silently ignore, if VM does not have the disk attached
	var newDataDisks []interface{}
	for _, v := range dataDisks {
		d := v.(map[string]interface{})
		vhdInfo, ok := d["vhd"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("convert vmData(vhd) to map error")
		}
		vhdURI := vhdInfo["uri"].(string)
		hashedVhdURI := MakeCRC32(vhdURI)
		if hasheddiskURI != hashedVhdURI {
			dataDisks = append(dataDisks, v)
		} else {
			diskURI = vhdURI
		}

	}

	// no disk found
	if diskURI == "" {
		glog.Warningf("azureDisk - disk with hash %s was not found atached on node %s", hasheddiskURI, nodeName)
		return nil
	}

	storageProfile["dataDisks"] = newDataDisks // -> store back
	payload := new(bytes.Buffer)
	err = json.NewEncoder(payload).Encode(fragment)
	if err != nil {
		return err
	}
	updateErr := c.common.updateArmVM(nodeName, payload)
	if updateErr != nil {
		return updateErr
	}

	// Wait for ARM to remove the disk from datadisks collection on the VM
	err = kwait.ExponentialBackoff(defaultBackOff, func() (bool, error) {
		attached, _, err := c.common.IsDiskAttached(hasheddiskURI, nodeName, false)
		if err == nil && !attached {
			return true, nil
		}
		return false, err
	})

	if err != nil {

		// confirm that the blob has no leases on it
		err = kwait.ExponentialBackoff(defaultBackOff, func() (bool, error) {
			var e error

			noLease, e := c.diskHasNoLease(diskURI)
			if e != nil {
				glog.Infof("azureDisk - failed to check if disk %s still has leases on it, we will assume clean-detach. Err:%s", diskURI, e.Error())
				return true, nil
			}

			if noLease {
				return true, nil
			}

			return false, nil
		})
	}

	if err != nil {
		glog.V(4).Infof("azureDisk - detached blob disk %s from node %s but was unable to confirm complete clean-detach during poll", diskURI, nodeName)
	} else {
		glog.V(4).Infof("azurDisk - detached blob disk %s from node %s", diskURI, nodeName)
	}

	return nil
}

//CreateBlobDisk : create a blob disk in a node
func (c *BlobDiskController) CreateBlobDisk(dataDiskName string, storageAccountType storage.SkuName, sizeGB int, forceStandAlone bool) (string, error) {
	glog.V(4).Infof("azureDisk - creating blob data disk named:%s on StorageAccountType:%s StandAlone:%v", dataDiskName, storageAccountType, forceStandAlone)

	var storageAccountName = ""
	var err error
	sizeBytes := 1024 * 1024 * 1024 * int64(sizeGB)
	vhdName := dataDiskName + ".vhd"
	totalVhdSize := sizeBytes + vhd.VHD_HEADER_SIZE

	if forceStandAlone {
		// we have to wait until the storage account is is created
		storageAccountName = "p" + MakeCRC32(c.common.subscriptionID+c.common.resourceGroup+dataDiskName)
		err = c.createStorageAccount(storageAccountName, storageAccountType, false)
		if err != nil {
			return "", err
		}
	} else {
		storageAccountName, err = c.findSANameForDisk(storageAccountType)
		if err != nil {
			return "", err
		}
	}

	blobSvc, err := c.getBlobSvcClient(storageAccountName)

	if err != nil {
		return "", err
	}

	tags := make(map[string]string)
	tags["created-by"] = "k8s-azure-DataDisk"

	glog.V(4).Infof("azureDisk - creating page blob for data disk %s\n", dataDiskName)

	if err := blobSvc.PutPageBlob(defaultContainerName, vhdName, totalVhdSize, tags); err != nil {
		glog.Infof("azureDisk - Failed to put page blob on account %s for data disk %s error was %s \n", storageAccountName, dataDiskName, err.Error())
		return "", err
	}

	vhdBytes, err := createVHDHeader(uint64(sizeBytes))

	if err != nil {
		glog.Infof("azureDisk - failed to load vhd asset for data disk %s size %v\n", dataDiskName, sizeGB)
		blobSvc.DeleteBlobIfExists(defaultContainerName, vhdName, nil)
		return "", err
	}

	headerBytes := vhdBytes[:vhd.VHD_HEADER_SIZE]

	if err = blobSvc.PutPage(defaultContainerName, vhdName, sizeBytes, totalVhdSize-1, azstorage.PageWriteTypeUpdate, headerBytes, nil); err != nil {
		_, _ = blobSvc.DeleteBlobIfExists(defaultContainerName, vhdName, nil)
		glog.Infof("azureDisk - failed to put header page for data disk %s on account %s error was %s\n", storageAccountName, dataDiskName, err.Error())
		return "", err
	}

	if !forceStandAlone {
		atomic.AddInt32(&c.accounts[storageAccountName].diskCount, 1)
	}

	host := fmt.Sprintf("https://%s.blob.%s", storageAccountName, c.common.storageEndpointSuffix)
	return fmt.Sprintf("%s/%s/%s", host, defaultContainerName, vhdName), nil
}

//DeleteBlobDisk : delete a blob disk from a node
func (c *BlobDiskController) DeleteBlobDisk(diskURI string, wasForced bool) error {
	storageAccountName, vhdName, err := diskNameandSANameFromURI(diskURI)
	if err != nil {
		return err
	}
	// if forced (as in one disk = one storage account)
	// delete the account completely
	if wasForced {
		return c.deleteStorageAccount(storageAccountName)
	}

	blobSvc, err := c.getBlobSvcClient(storageAccountName)
	if err != nil {
		return err
	}

	glog.V(2).Infof("azureDisk - About to delete vhd file %s on storage account %s container %s", vhdName, storageAccountName, defaultContainerName)

	_, err = blobSvc.DeleteBlobIfExists(defaultContainerName, vhdName, nil)

	if c.accounts[storageAccountName].diskCount == -1 {
		if diskCount, err := c.getDiskCount(storageAccountName); err != nil {
			c.accounts[storageAccountName].diskCount = int32(diskCount)
		} else {
			glog.Warningf("azureDisk - failed to get disk count for %s however the delete disk operation was ok", storageAccountName)
			return nil // we have failed to aquire a new count. not an error condition
		}
	}
	atomic.AddInt32(&c.accounts[storageAccountName].diskCount, -1)
	return err
}

func (c *BlobDiskController) diskHasNoLease(diskURI string) (bool, error) {
	if !strings.Contains(diskURI, defaultContainerName) {
		// if the disk was attached via PV (with possibility of existing out side
		// this RG), we will have to drop this check, as we are not sure if we can
		// get keys for this account
		glog.Infof("azureDisk - assumed that disk %s is not provisioned via PV and will not check if it has leases on it", diskURI)
		return true, nil
	}

	diskStorageAccount, vhdName, err := diskNameandSANameFromURI(diskURI)
	if err != nil {
		glog.Infof("azureDisk - could not check if disk %s has a lease on it (diskNameandSANameFromURI):%s", diskURI, err.Error())
		return false, err
	}

	blobSvc, e := c.getBlobSvcClient(diskStorageAccount)
	if e != nil {
		glog.Infof("azureDisk - could not check if disk %s has a lease on it (getBlobSvcClient):%s", diskURI, err.Error())
		return false, e
	}

	metaMap := make(map[string]string)
	metaMap["azureddheck"] = "ok"
	e = blobSvc.SetBlobMetadata(defaultContainerName, vhdName, metaMap, nil)
	if e != nil {
		// disk has lease on it or does not exist, in both cases it something we can not go forward with
		return false, nil
	}
	return true, nil
}

// Init tries best effort to ensure that 2 accounts standard/premium were created
// to be used by shared blob disks. This to increase the speed pvc provisioning (in most of cases)
func (c *BlobDiskController) init() error {
	if !c.shouldInit() {
		return nil
	}

	c.setUniqueStrings()

	// get accounts
	accounts, err := c.getAllStorageAccounts()
	if err != nil {
		return err
	}
	c.accounts = accounts

	if len(c.accounts) == 0 {
		counter := 1
		for counter <= storageAccountsCountInit {

			accountType := storage.PremiumLRS
			if n := math.Mod(float64(counter), 2); n == 0 {
				accountType = storage.StandardLRS
			}

			// We don't really care if these calls failed
			// at this stage, we are trying to ensure 2 accounts (Standard/Premium)
			// are there ready for PVC creation

			// if we failed here, the accounts will be created in the process
			// of creating PVC

			// nor do we care if they were partially created, as the entire
			// account creation process is idempotent
			go func(thisNext int) {
				newAccountName := getAccountNameForNum(thisNext)

				glog.Infof("azureDisk - BlobDiskController init process  will create new storageAccount:%s type:%s", newAccountName, accountType)
				err := c.createStorageAccount(newAccountName, accountType, true)
				// TODO return created and error from
				if err != nil {
					glog.Infof("azureDisk - BlobDiskController init: create account %s with error:%s", newAccountName, err.Error())

				} else {
					glog.Infof("azureDisk - BlobDiskController init: created account %s", newAccountName)
				}
			}(counter)
			counter = counter + 1
		}
	}

	return nil
}

//Sets unique strings to be used as accountnames && || blob containers names
func (c *BlobDiskController) setUniqueStrings() {
	uniqueString := c.common.resourceGroup + c.common.location + c.common.subscriptionID
	hash := MakeCRC32(uniqueString)
	//used to generate a unqie container name used by this cluster PVC
	defaultContainerName = hash

	storageAccountNamePrefix = fmt.Sprintf(storageAccountNameTemplate, hash)
	// Used to filter relevant accounts (accounts used by shared PVC)
	storageAccountNameMatch = storageAccountNamePrefix
	// Used as a template to create new names for relevant accounts
	storageAccountNamePrefix = storageAccountNamePrefix + "%s"
}
func (c *BlobDiskController) getStorageAccountKey(SAName string) (string, error) {
	if account, exists := c.accounts[SAName]; exists && account.key != "" {
		return c.accounts[SAName].key, nil
	}
	listKeysResult, err := c.common.cloud.StorageAccountClient.ListKeys(c.common.resourceGroup, SAName)
	if err != nil {
		return "", err
	}
	if listKeysResult.Keys == nil {
		return "", fmt.Errorf("azureDisk - empty listKeysResult in storage account:%s keys", SAName)
	}
	for _, v := range *listKeysResult.Keys {
		if v.Value != nil && *v.Value == "key1" {
			if _, ok := c.accounts[SAName]; !ok {
				glog.Warningf("azureDisk - account %s was not cached while getting keys", SAName)
				return *v.Value, nil
			}
		}

		c.accounts[SAName].key = *v.Value
		return c.accounts[SAName].key, nil
	}

	return "", fmt.Errorf("couldn't find key named key1 in storage account:%s keys", SAName)
}

func (c *BlobDiskController) getBlobSvcClient(SAName string) (azstorage.BlobStorageClient, error) {
	key := ""
	var client azstorage.Client
	var blobSvc azstorage.BlobStorageClient
	var err error
	if key, err = c.getStorageAccountKey(SAName); err != nil {
		return blobSvc, err
	}

	if client, err = azstorage.NewBasicClient(SAName, key); err != nil {
		return blobSvc, err
	}

	blobSvc = client.GetBlobService()
	return blobSvc, nil
}

func (c *BlobDiskController) ensureDefaultContainer(storageAccountName string) error {
	var err error
	var blobSvc azstorage.BlobStorageClient

	// short circut the check via local cache
	// we are forgiving the fact that account may not be in cache yet
	if v, ok := c.accounts[storageAccountName]; ok && v.defaultContainerCreated {
		return nil
	}

	// not cached, check existance and readiness
	bExist, provisionState, _ := c.getStorageAccountState(storageAccountName)

	// account does not exist
	if !bExist {
		return fmt.Errorf("azureDisk - account %s does not exist while trying to create/ensure default container", storageAccountName)
	}

	// account exists but not ready yet
	if provisionState != storage.Succeeded {
		// we don't want many attempts to validate the account readiness
		// here hence we are locking
		counter := 1
		for swapped := atomic.CompareAndSwapInt32(&c.accounts[storageAccountName].isValidating, 0, 1); swapped != true; {
			time.Sleep(3 * time.Second)
			counter = counter + 1
			// check if we passed the max sleep
			if counter >= 20 {
				return fmt.Errorf("azureDisk - timeout waiting to aquire lock to validate account:%s readiness", storageAccountName)
			}
		}

		// swapped
		defer func() {
			c.accounts[storageAccountName].isValidating = 0
		}()

		// short circut the check again.
		if v, ok := c.accounts[storageAccountName]; ok && v.defaultContainerCreated {
			return nil
		}

		err = kwait.ExponentialBackoff(defaultBackOff, func() (bool, error) {
			_, provisionState, err := c.getStorageAccountState(storageAccountName)

			if err != nil {
				glog.V(4).Infof("azureDisk - GetStorageAccount:%s err %s", storageAccountName, err.Error())
				return false, err
			}

			if provisionState == storage.Succeeded {
				return true, nil
			}

			glog.V(4).Infof("azureDisk - GetStorageAccount:%s not ready yet", storageAccountName)
			// leave it for next loop/sync loop
			return false, fmt.Errorf("azureDisk - Account %s has not been flagged Succeeded by ARM", storageAccountName)
		})
		// we have failed to ensure that account is ready for us to create
		// the default vhd container
		if err != nil {
			return err
		}
	}

	if blobSvc, err = c.getBlobSvcClient(storageAccountName); err != nil {
		return err
	}

	bCreated, err := blobSvc.CreateContainerIfNotExists(defaultContainerName, azstorage.ContainerAccessType(""))
	if err != nil {
		return err
	}
	if bCreated {
		glog.V(2).Infof("azureDisk - storage account:%s had no default container(%s) and it was created \n", storageAccountName, defaultContainerName)
	}

	// flag so we no longer have to check on ARM
	c.accounts[storageAccountName].defaultContainerCreated = true
	return nil
}

// Gets Disk counts per storage account
func (c *BlobDiskController) getDiskCount(SAName string) (int, error) {
	// if we have it in cache
	if c.accounts[SAName].diskCount != -1 {
		return int(c.accounts[SAName].diskCount), nil
	}

	var err error
	var blobSvc azstorage.BlobStorageClient

	if err = c.ensureDefaultContainer(SAName); err != nil {
		return 0, err
	}

	if blobSvc, err = c.getBlobSvcClient(SAName); err != nil {
		return 0, err
	}
	params := azstorage.ListBlobsParameters{}

	response, err := blobSvc.ListBlobs(defaultContainerName, params)
	if err != nil {
		return 0, err
	}
	glog.V(4).Infof("azure-Disk -  refreshed data count for account %s and found %v", SAName, len(response.Blobs))
	c.accounts[SAName].diskCount = int32(len(response.Blobs))

	return int(c.accounts[SAName].diskCount), nil
}

// shouldInit ensures that we only init the plugin once
// and we only do that in the controller

func (c *BlobDiskController) shouldInit() bool {
	if os.Args[0] == "kube-controller-manager" || (os.Args[0] == "/hyperkube" && os.Args[1] == "controller-manager") {
		swapped := atomic.CompareAndSwapInt64(&initFlag, 0, 1)
		if swapped {
			return true
		}
	}
	return false
}

func (c *BlobDiskController) getAllStorageAccounts() (map[string]*storageAccountState, error) {
	accountListResult, err := c.common.cloud.StorageAccountClient.List()
	if err != nil {
		return nil, err
	}
	if accountListResult.Value == nil {
		return nil, fmt.Errorf("azureDisk - empty accountListResult")
	}

	accounts := make(map[string]*storageAccountState)
	for _, v := range *accountListResult.Value {
		if strings.Index(*v.Name, storageAccountNameMatch) != 0 {
			continue
		}
		if v.Name == nil || v.Sku == nil {
			glog.Infof("azureDisk - accountListResult Name or Sku is nil")
			continue
		}
		glog.Infof("azureDisk - identified account %s as part of shared PVC accounts", *v.Name)

		sastate := &storageAccountState{
			name:      *v.Name,
			saType:    (*v.Sku).Name,
			diskCount: -1,
		}

		accounts[*v.Name] = sastate
	}

	return accounts, nil
}

func (c *BlobDiskController) createStorageAccount(storageAccountName string, storageAccountType storage.SkuName, checkMaxAccounts bool) error {
	bExist, _, _ := c.getStorageAccountState(storageAccountName)
	if bExist {
		newAccountState := &storageAccountState{
			diskCount: -1,
			saType:    storageAccountType,
			name:      storageAccountName,
		}

		c.addAccountState(storageAccountName, newAccountState)
	}
	// Account Does not exist
	if !bExist {
		if len(c.accounts) == maxStorageAccounts && checkMaxAccounts {
			return fmt.Errorf("azureDisk - can not create new storage account, current storage accounts count:%v Max is:%v", len(c.accounts), maxStorageAccounts)
		}

		glog.V(2).Infof("azureDisk - Creating storage account %s type %s \n", storageAccountName, string(storageAccountType))

		tag := "azure-dd"
		cp := storage.AccountCreateParameters{
			Sku:      &storage.Sku{Name: storageAccountType},
			Tags:     &map[string]*string{"created-by": &tag},
			Location: to.StringPtr(c.common.location)}
		cancel := make(chan struct{})

		_, err := c.common.cloud.StorageAccountClient.Create(c.common.resourceGroup, storageAccountName, cp, cancel)
		if err != nil {
			return fmt.Errorf(fmt.Sprintf("Create Storage Account: %s, error: %s", storageAccountName, err))
		}

		newAccountState := &storageAccountState{
			diskCount: -1,
			saType:    storageAccountType,
			name:      storageAccountName,
		}

		c.addAccountState(storageAccountName, newAccountState)
	}

	if !bExist {
		// SA Accounts takes time to be provisioned
		// so if this account was just created allow it sometime
		// before polling
		glog.V(4).Infof("azureDisk - storage account %s was just created, allowing time before polling status")
		time.Sleep(25 * time.Second) // as observed 25 is the average time for SA to be provisioned
	}

	// finally, make sure that we default container is created
	// before handing it back over
	return c.ensureDefaultContainer(storageAccountName)
}

// finds a new suitable storageAccount for this disk
func (c *BlobDiskController) findSANameForDisk(storageAccountType storage.SkuName) (string, error) {
	maxDiskCount := maxDisksPerStorageAccounts
	SAName := ""
	totalDiskCounts := 0
	countAccounts := 0 // account of this type.
	for _, v := range c.accounts {
		// filter out any stand-alone disks/accounts
		if strings.Index(v.name, storageAccountNameMatch) != 0 {
			continue
		}

		// note: we compute avge stratified by type.
		// this to enable user to grow per SA type to avoid low
		//avg utilization on one account type skewing all data.

		if v.saType == storageAccountType {
			// compute average
			dCount, err := c.getDiskCount(v.name)
			if err != nil {
				return "", err
			}
			totalDiskCounts = totalDiskCounts + dCount
			countAccounts = countAccounts + 1
			// empty account
			if dCount == 0 {
				glog.V(4).Infof("azureDisk - account %s identified for a new disk  is because it has 0 allocated disks", v.name)
				return v.name, nil // shortcircut, avg is good and no need to adjust
			}
			// if this account is less allocated
			if dCount < maxDiskCount {
				maxDiskCount = dCount
				SAName = v.name
			}
		}
	}

	// if we failed to find storageaccount
	if SAName == "" {

		glog.Infof("azureDisk - failed to identify a suitable account for new disk and will attempt to create new account")
		SAName = getAccountNameForNum(c.getNextAccountNum())
		err := c.createStorageAccount(SAName, storageAccountType, true)
		if err != nil {
			return "", err
		}
		return SAName, nil
	}

	disksAfter := totalDiskCounts + 1 // with the new one!

	avgUtilization := float64(disksAfter) / float64(countAccounts*maxDisksPerStorageAccounts)
	aboveAvg := (avgUtilization > storageAccountUtilizationBeforeGrowing)

	// avg are not create and we should craete more accounts if we can
	if aboveAvg && countAccounts < maxStorageAccounts {
		glog.Infof("azureDisk - shared storageAccounts utilzation(%v) >  grow-at-avg-utilization (%v). New storage account will be created", avgUtilization, storageAccountUtilizationBeforeGrowing)
		SAName = getAccountNameForNum(c.getNextAccountNum())
		err := c.createStorageAccount(SAName, storageAccountType, true)
		if err != nil {
			return "", err
		}
		return SAName, nil
	}

	// avergates are not ok and we are at capacity(max storage accounts allowed)
	if aboveAvg && countAccounts == maxStorageAccounts {

		glog.Infof("azureDisk - shared storageAccounts utilzation(%v) > grow-at-avg-utilization (%v). But k8s maxed on SAs for PVC(%v). k8s will now exceed  grow-at-avg-utilization without adding accounts",
			avgUtilization, storageAccountUtilizationBeforeGrowing, maxStorageAccounts)
	}

	// we found a  storage accounts && [ avg are ok || we reached max sa count ]
	return SAName, nil
}
func (c *BlobDiskController) getNextAccountNum() int {
	max := 0

	for k := range c.accounts {
		// filter out accounts that are for standalone
		if strings.Index(k, storageAccountNameMatch) != 0 {
			continue
		}
		num := getAccountNumFromName(k)
		if num > max {
			max = num
		}
	}

	return max + 1
}

func (c *BlobDiskController) deleteStorageAccount(storageAccountName string) error {
	resp, err := c.common.cloud.StorageAccountClient.Delete(c.common.resourceGroup, storageAccountName)
	if err != nil {
		return fmt.Errorf("azureDisk - Delete of storage account '%s' failed with status %s...%v", storageAccountName, resp.Status, err)
	}

	c.removeAccountState(storageAccountName)

	glog.Infof("azureDisk - Storage Account %s was deleted", storageAccountName)
	return nil
}

//Gets storage account exist, provisionStatus, Error if any
func (c *BlobDiskController) getStorageAccountState(storageAccountName string) (bool, storage.ProvisioningState, error) {
	account, err := c.common.cloud.StorageAccountClient.GetProperties(c.common.resourceGroup, storageAccountName)
	if err != nil {
		return false, "", err
	}
	return true, account.AccountProperties.ProvisioningState, nil
}

func (c *BlobDiskController) addAccountState(key string, state *storageAccountState) {
	accountsLock.Lock()
	defer accountsLock.Unlock()

	if _, ok := c.accounts[key]; !ok {
		c.accounts[key] = state
	}
}

func (c *BlobDiskController) removeAccountState(key string) {
	accountsLock.Lock()
	defer accountsLock.Unlock()
	delete(c.accounts, key)
}

// pads account num with zeros as needed
func getAccountNameForNum(num int) string {
	sNum := strconv.Itoa(num)
	missingZeros := 3 - len(sNum)
	strZero := ""
	for missingZeros > 0 {
		strZero = strZero + "0"
		missingZeros = missingZeros - 1
	}

	sNum = strZero + sNum
	return fmt.Sprintf(storageAccountNamePrefix, sNum)
}

func getAccountNumFromName(accountName string) int {
	nameLen := len(accountName)
	num, _ := strconv.Atoi(accountName[nameLen-3:])

	return num
}

func createVHDHeader(size uint64) ([]byte, error) {
	h := vhd.CreateFixedHeader(size, &vhd.VHDOptions{})
	b := new(bytes.Buffer)
	err := binary.Write(b, binary.BigEndian, h)
	if err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func diskNameandSANameFromURI(diskURI string) (string, string, error) {
	uri, err := url.Parse(diskURI)
	if err != nil {
		return "", "", err
	}

	hostName := uri.Host
	storageAccountName := strings.Split(hostName, ".")[0]

	segments := strings.Split(uri.Path, "/")
	diskNameVhd := segments[len(segments)-1]

	return storageAccountName, diskNameVhd, nil
}
