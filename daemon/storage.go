package daemon

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	dockertypes "github.com/docker/engine-api/types"
	"github.com/golang/glog"
	"github.com/hyperhq/hyperd/storage"
	"github.com/hyperhq/hyperd/storage/aufs"
	dm "github.com/hyperhq/hyperd/storage/devicemapper"
	"github.com/hyperhq/hyperd/storage/overlay"
	"github.com/hyperhq/hyperd/storage/vbox"
	"github.com/hyperhq/hyperd/utils"
	runv "github.com/hyperhq/runv/api"
	apitypes "github.com/hyperhq/hyperd/types"
	"github.com/hyperhq/hyperd/daemon/daemondb"
)

const (
	DEFAULT_DM_POOL      string = "hyper-volume-pool"
	DEFAULT_DM_POOL_SIZE int    = 20971520 * 512
	DEFAULT_DM_DATA_LOOP string = "/dev/loop6"
	DEFAULT_DM_META_LOOP string = "/dev/loop7"
	DEFAULT_DM_VOL_SIZE  int    = 2 * 1024 * 1024 * 1024
	DEFAULT_VOL_FS              = "ext4"
	DEFAULT_VOL_MKFS            = "mkfs.ext4"
)

type Storage interface {
	Type() string
	RootPath() string

	Init() error
	CleanUp() error

	PrepareContainer(ci *LegacyContainer, sharedir string) error
	CleanupContainer(id, sharedDir string) error
	InjectFile(src io.Reader, containerId, target, rootDir string, perm, uid, gid int) error
	CreateVolume(podId string, spec *apitypes.UserVolume) error
	RemoveVolume(podId string, record []byte) error
}

var StorageDrivers map[string]func(*dockertypes.Info, *daemondb.DaemonDB) (Storage, error) = map[string]func(*dockertypes.Info, *daemondb.DaemonDB) (Storage, error){
	"devicemapper": DMFactory,
	"aufs":         AufsFactory,
	"overlay":      OverlayFsFactory,
	"vbox":         VBoxStorageFactory,
}

func StorageFactory(sysinfo *dockertypes.Info, db *daemondb.DaemonDB) (Storage, error) {
	if factory, ok := StorageDrivers[sysinfo.Driver]; ok {
		return factory(sysinfo, db)
	}
	return nil, fmt.Errorf("hyperd can not support docker's backing storage: %s", sysinfo.Driver)
}

func ProbeExistingVolume(v *apitypes.UserVolume, sharedDir string) (*runv.VolumeDescription, error) {
	if v == nil || v.Source == "" { //do not create volume in this function, it depends on storage driver.
		return nil, fmt.Errorf("can not generate volume info from %v", v)
	}

	var err error = nil
	vol := &runv.VolumeDescription{
		Name: v.Name,
		Source: v.Source,
		Format: v.Format,
		Fstype: v.Fstype,
		Options: &runv.VolumeOption{
			User: v.Option.User,
			Monitors: v.Option.Monitors,
			Keyring: v.Option.Keyring,
		},
	}

	if v.Format == "vfs" {
		vol.Fstype = "dir"
		vol.Source, err = storage.MountVFSVolume(v.Source, sharedDir)
		if err != nil {
			return nil, err
		}
		glog.V(1).Infof("dir %s is bound to %s", v.Source, vol.Source)
	} else if v.Format == "raw" && v.Fstype == "" {
		vol.Fstype, err = dm.ProbeFsType(v.Source)
		if err != nil {
			vol.Fstype = DEFAULT_VOL_FS
		}
	}

	return vol, nil
}

func CleanupExistingVolume(fstype, filepath, sharedDir string) error {
	if fstype == "dir" {
		return storage.UmountVFSVolume(filepath, sharedDir)
	}
	return dm.UnmapVolume(filepath)
}

type DevMapperStorage struct {
	db          *daemondb.DaemonDB
	CtnPoolName string
	VolPoolName string
	DevPrefix   string
	FsType      string
	rootPath    string
	DmPoolData  *dm.DeviceMapper
}

func DMFactory(sysinfo *dockertypes.Info, db *daemondb.DaemonDB) (Storage, error) {
	driver := &DevMapperStorage{
		db: db,
	}

	driver.VolPoolName = DEFAULT_DM_POOL

	for _, pair := range sysinfo.DriverStatus {
		if pair[0] == "Pool Name" {
			driver.CtnPoolName = pair[1]
		}
		if pair[0] == "Backing Filesystem" {
			if strings.Contains(pair[1], "ext") {
				driver.FsType = "ext4"
			} else if strings.Contains(pair[1], "xfs") {
				driver.FsType = "xfs"
			} else {
				driver.FsType = "dir"
			}
			break
		}
	}
	driver.DevPrefix = driver.CtnPoolName[:strings.Index(driver.CtnPoolName, "-pool")]
	driver.rootPath = filepath.Join(utils.HYPER_ROOT, "devicemapper")
	return driver, nil
}

func (dms *DevMapperStorage) Type() string {
	return "devicemapper"
}

func (dms *DevMapperStorage) RootPath() string {
	return dms.rootPath
}

func (dms *DevMapperStorage) Init() error {
	dmPool := dm.DeviceMapper{
		Datafile:         filepath.Join(utils.HYPER_ROOT, "lib") + "/data",
		Metadatafile:     filepath.Join(utils.HYPER_ROOT, "lib") + "/metadata",
		DataLoopFile:     DEFAULT_DM_DATA_LOOP,
		MetadataLoopFile: DEFAULT_DM_META_LOOP,
		PoolName:         dms.VolPoolName,
		Size:             DEFAULT_DM_POOL_SIZE,
	}
	dms.DmPoolData = &dmPool
	rand.Seed(time.Now().UnixNano())

	// Prepare the DeviceMapper storage
	return dm.CreatePool(&dmPool)
}

func (dms *DevMapperStorage) CleanUp() error {
	return dm.DMCleanup(dms.DmPoolData)
}

func (dms *DevMapperStorage) PrepareContainer(ci *LegacyContainer, sharedDir string) error {
	if err := dm.CreateNewDevice(ci.mountID, dms.DevPrefix, dms.RootPath()); err != nil {
		return err
	}
	devFullName, err := dm.MountContainerToSharedDir(ci.mountID, sharedDir, dms.DevPrefix)
	if err != nil {
		glog.Error("got error when mount container to share dir ", err.Error())
		return err
	}
	fstype, err := dm.ProbeFsType(devFullName)
	if err != nil {
		fstype = "ext4"
	}

	ci.rootfs = "/rootfs"
	ci.fstype = fstype
	ci.ApiContainer.Image = devFullName

	return nil
}

func (dms *DevMapperStorage) CleanupContainer(id, sharedDir string) error {
	devFullName, err := dm.MountContainerToSharedDir(id, sharedDir, dms.DevPrefix)
	if err != nil {
		glog.Error("got error when mount container to share dir ", err.Error())
		return err
	}

	return dm.UnmapVolume(devFullName)
}

func (dms *DevMapperStorage) InjectFile(src io.Reader, containerId, target, rootDir string, perm, uid, gid int) error {
	return dm.InjectFile(src, containerId, dms.DevPrefix, target, rootDir, perm, uid, gid)
}

func (dms *DevMapperStorage) getPersistedId(podId, volName string) (int, error) {
	vols, err := dms.db.ListPodVolumes(podId)
	if err != nil {
		return -1, err
	}

	dev_id := 0
	for _, vol := range vols {
		fields := strings.Split(string(vol), ":")
		if fields[0] == volName {
			dev_id, _ = strconv.Atoi(fields[1])
		}
	}
	return dev_id, nil
}

func (dms *DevMapperStorage) CreateVolume(podId string, spec *apitypes.UserVolume) error {
	var err error

	deviceName := fmt.Sprintf("%s-%s-%s", dms.VolPoolName, podId, spec.Name)
	dev_id, _ := dms.getPersistedId(podId, deviceName)
	glog.Infof("DeviceID is %d", dev_id)

	restore := dev_id > 0

	for {
		if !restore {
			dev_id = dms.randDevId()
		}
		dev_id_str := strconv.Itoa(dev_id)

		err = dm.CreateVolume(dms.VolPoolName, deviceName, dev_id_str, DEFAULT_VOL_MKFS, DEFAULT_DM_VOL_SIZE, restore)
		if err != nil && !restore && strings.Contains(err.Error(), "failed: File exists") {
			glog.V(1).Infof("retry for dev_id #%d creating collision: %v", dev_id, err)
			continue
		} else if err != nil {
			glog.V(1).Infof("failed to create dev_id #%d: %v", dev_id, err)
			return err
		}

		glog.V(3).Infof("device (%d) created (restore:%v) for %s: %s", dev_id, restore, podId, deviceName)
		dms.db.UpdatePodVolume(podId, deviceName, []byte(fmt.Sprintf("%s:%s", deviceName, dev_id_str)))
		break
	}

	fstype := DEFAULT_VOL_FS
	if !restore {
		if spec.Fstype == "" {
			fstype, err = dm.ProbeFsType("/dev/mapper/" + deviceName)
			if err != nil {
				fstype = DEFAULT_VOL_FS
			}
		} else {
			fstype = spec.Fstype
		}
	}

	glog.V(1).Infof("volume %s created with dm as %s", spec.Name, deviceName)

	spec.Source = filepath.Join("/dev/mapper/", deviceName)
	spec.Format = "raw"
	spec.Fstype = fstype

	return nil
}

func (dms *DevMapperStorage) RemoveVolume(podId string, record []byte) error {
	fields := strings.Split(string(record), ":")
	dev_id, _ := strconv.Atoi(fields[1])
	if err := dm.DeleteVolume(dms.DmPoolData, dev_id); err != nil {
		glog.Error(err.Error())
		return err
	}
	return nil
}

func (dms *DevMapperStorage) randDevId() int {
	return rand.Intn(1<<24-1) + 1 // 0 reserved for pool device
}

type AufsStorage struct {
	rootPath string
}

func AufsFactory(sysinfo *dockertypes.Info, _ *daemondb.DaemonDB) (Storage, error) {
	driver := &AufsStorage{}
	for _, pair := range sysinfo.DriverStatus {
		if pair[0] == "Root Dir" {
			driver.rootPath = pair[1]
		}
	}
	return driver, nil
}

func (a *AufsStorage) Type() string {
	return "aufs"
}

func (a *AufsStorage) RootPath() string {
	return a.rootPath
}

func (*AufsStorage) Init() error { return nil }

func (*AufsStorage) CleanUp() error { return nil }

func (a *AufsStorage) PrepareContainer(ci *LegacyContainer, sharedDir string) error {
	_, err := aufs.MountContainerToSharedDir(ci.mountID, a.RootPath(), sharedDir, "")
	if err != nil {
		glog.Error("got error when mount container to share dir ", err.Error())
		return err
	}

	devFullName := "/" + ci.mountID + "/rootfs"
	ci.ApiContainer.Image = devFullName
	ci.fstype = "dir"

	return nil
}

func (a *AufsStorage) CleanupContainer(id, sharedDir string) error {
	return aufs.Unmount(filepath.Join(sharedDir, id, "rootfs"))
}

func (a *AufsStorage) InjectFile(src io.Reader, containerId, target, rootDir string, perm, uid, gid int) error {
	return storage.FsInjectFile(src, containerId, target, rootDir, perm, uid, gid)
}

func (a *AufsStorage) CreateVolume(podId string, spec *apitypes.UserVolume) error {
	volName, err := storage.CreateVFSVolume(podId, spec.Name)
	if err != nil {
		return err
	}
	spec.Source = volName
	spec.Format = "vfs"
	spec.Fstype = "dir"
	return  nil
}

func (a *AufsStorage) RemoveVolume(podId string, record []byte) error {
	return nil
}

type OverlayFsStorage struct {
	rootPath string
}

func OverlayFsFactory(_ *dockertypes.Info, _ *daemondb.DaemonDB) (Storage, error) {
	driver := &OverlayFsStorage{
		rootPath: filepath.Join(utils.HYPER_ROOT, "overlay"),
	}
	return driver, nil
}

func (o *OverlayFsStorage) Type() string {
	return "overlay"
}

func (o *OverlayFsStorage) RootPath() string {
	return o.rootPath
}

func (*OverlayFsStorage) Init() error { return nil }

func (*OverlayFsStorage) CleanUp() error { return nil }

func (o *OverlayFsStorage) PrepareContainer(ci *LegacyContainer, sharedDir string) error {
	_, err := overlay.MountContainerToSharedDir(ci.mountID, o.RootPath(), sharedDir, "")
	if err != nil {
		glog.Error("got error when mount container to share dir ", err.Error())
		return err
	}

	devFullName := "/" + ci.mountID + "/rootfs"
	ci.ApiContainer.Image = devFullName
	ci.fstype = "dir"

	return nil
}

func (o *OverlayFsStorage) CleanupContainer(id, sharedDir string) error {
	return syscall.Unmount(filepath.Join(sharedDir, id, "rootfs"), 0)
}

func (o *OverlayFsStorage) InjectFile(src io.Reader, containerId, target, rootDir string, perm, uid, gid int) error {
	return storage.FsInjectFile(src, containerId, target, rootDir, perm, uid, gid)
}

func (o *OverlayFsStorage) CreateVolume(podId string, spec *apitypes.UserVolume) error {
	volName, err := storage.CreateVFSVolume(podId, spec.Name)
	if err != nil {
		return err
	}
	spec.Source = volName
	spec.Format = "vfs"
	spec.Fstype = "dir"
	return  nil
}

func (o *OverlayFsStorage) RemoveVolume(podId string, record []byte) error {
	return nil
}

type VBoxStorage struct {
	rootPath string
}

func VBoxStorageFactory(_ *dockertypes.Info, _ *daemondb.DaemonDB) (Storage, error) {
	driver := &VBoxStorage{
		rootPath: filepath.Join(utils.HYPER_ROOT, "vbox"),
	}
	return driver, nil
}

func (v *VBoxStorage) Type() string {
	return "vbox"
}

func (v *VBoxStorage) RootPath() string {
	return v.rootPath
}

func (*VBoxStorage) Init() error { return nil }

func (*VBoxStorage) CleanUp() error { return nil }

func (v *VBoxStorage) PrepareContainer(ci *LegacyContainer, sharedDir string) error {
	devFullName, err := vbox.MountContainerToSharedDir(ci.mountID, v.RootPath(), "")
	if err != nil {
		glog.Error("got error when mount container to share dir ", err.Error())
		return err
	}

	ci.rootfs = "/rootfs"
	ci.ApiContainer.Image = devFullName
	ci.fstype = "ext4"

	return nil
}

func (v *VBoxStorage) CleanupContainer(id, sharedDir string) error {
	return nil
}

func (v *VBoxStorage) InjectFile(src io.Reader, containerId, target, rootDir string, perm, uid, gid int) error {
	return errors.New("vbox storage driver does not support file insert yet")
}

func (v *VBoxStorage) CreateVolume(podId string, spec *apitypes.UserVolume) error {
	volName, err := storage.CreateVFSVolume(podId, spec.Name)
	if err != nil {
		return err
	}
	spec.Source = volName
	spec.Format = "vfs"
	spec.Fstype = "dir"
	return  nil
}

func (v *VBoxStorage) RemoveVolume(podId string, record []byte) error {
	return nil
}
