package daemon

import (
	"sync"
	"net"
	"fmt"
	"path"
	"encoding/json"

	"github.com/hyperhq/runv/hypervisor"
	"github.com/hyperhq/runv/hypervisor/pod"
	"github.com/hyperhq/runv/hypervisor/types"
	apitypes "github.com/hyperhq/hyperd/types"
)

type LegacyContainer struct {
	ApiContainer *apitypes.Container
	CreatedAt    int64
	mountID      string
	fstype       string
	rootfs       string
	initialize   bool
}

type LegacyPod struct {
	Id        string
	CreatedAt int64
	PodStatus *hypervisor.PodStatus
	VM        *hypervisor.Vm

	containers []*LegacyContainer
	volumes    map[string]*hypervisor.VolumeInfo

	transiting chan bool
	sync.RWMutex

	/*----- Refactoring, New fields will be add/move to below area -----*/
	Spec *apitypes.UserPod
}

func NewLegacyPod(podSpec *apitypes.UserPod, id string, data interface{}) (*LegacyPod, error) {
	var err error

	p := &LegacyPod{
		Id:         id,
		CreatedAt:  time.Now().Unix(),
		volumes:    make(map[string]*hypervisor.VolumeInfo),
		transiting: make(chan bool, 1),
	}

	// fill one element in transit chan, only one parallel op is allowed
	p.transiting <- true

	return p, nil
}

//func (p *LegacyPod) cleanupMountsAndFiles(sd Storage, sharedDir string) {
//	for i := range p.PodStatus.Containers {
//		mountId := p.containers[i].mountID
//		sd.CleanupContainer(mountId, sharedDir)
//	}
//}
//
//func (p *LegacyPod) cleanupVolumes(sd Storage, sharedDir string) {
//	for _, v := range p.volumes {
//		CleanupExistingVolume(v.Fstype, v.Filepath, sharedDir)
//	}
//}
//
//func (p *LegacyPod) Cleanup(daemon *Daemon) {
//	p.PodStatus.Vm = ""
//
//	if p.VM == nil {
//		return
//	}
//
//	sharedDir := path.Join(hypervisor.BaseDir, p.VM.Id, hypervisor.ShareDirTag)
//	p.VM = nil
//
//	daemon.db.DeleteVMByPod(p.Id)
//
//	p.cleanupVolumes(daemon.Storage, sharedDir)
//	p.cleanupMountsAndFiles(daemon.Storage, sharedDir)
//	p.cleanupEtcHosts()
//
//	p.PodStatus.CleanupExec()
//
//	if p.PodStatus.Status == types.S_POD_NONE {
//		daemon.RemovePodResource(p)
//	}
//}

