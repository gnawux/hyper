package daemon

import (
	"fmt"
	"os"
	"path"

	dockertypes "github.com/docker/engine-api/types"
	"github.com/golang/glog"
	"github.com/hyperhq/hyperd/utils"
	"github.com/hyperhq/runv/hypervisor/types"
)

const (
	E_NOT_FOUND       = -2
	E_UNDER_OPERATION = -1
	E_OK              = 0
)

func (daemon *Daemon) RemovePod(podId string) (int, string, error) {
	var (
		code  = E_OK
		cause = ""
		err   error
	)

	p, ok := daemon.PodList.Get(podId)
	if !ok {
		return E_NOT_FOUND, "", fmt.Errorf("Can not find that Pod(%s)", podId)
	}

	daemon.PodList.Delete(podId)

	if p.IsAlive() {
		glog.V(1).Infof("remove pod %s, stop it firstly", podId)
		p.Stop(5)
	}

	p.Remove()

	return code, cause, err
}

func (daemon *Daemon) RemoveContainer(nameOrId string) error {
	p, id, err :=  daemon.PodList.GetByContainerIdOrName(nameOrId)
	if err != nil {
		glog.Error(err)
		return err
	}

	daemon.PodList.DeleteContainer(id, p.ContainerId2Name(id))

	if p.ContainerIsRunning(id) {
		err := p.StopContainer(id, 5)
		if err != nil {
			glog.Error(err)
			return err
		}
	}

	return p.RemoveContainer(id, true)
}

func (daemon *Daemon) UnlinkContainerFromPod(nameOrId string) error {
	p, id, err :=  daemon.PodList.GetByContainerIdOrName(nameOrId)
	if err != nil {
		glog.Error(err)
		return err
	}

	daemon.PodList.DeleteContainer(id, p.ContainerId2Name(id))

	if p.ContainerIsRunning(id) {
		err := p.StopContainer(id, 5)
		if err != nil {
			glog.Error(err)
			return err
		}
	}

	return p.RemoveContainer(id, false)
}

func (p *LegacyPod) ShouldWaitCleanUp() bool {
	return p.VM != nil
}

func (daemon *Daemon) RemovePodResource(p *LegacyPod) {
	if p.ShouldWaitCleanUp() {
		glog.V(3).Infof("pod %s should wait clean up before being purged", p.Id)
		p.PodStatus.Status = types.S_POD_NONE
		return
	}
	glog.V(3).Infof("pod %s is being purged", p.Id)

	os.RemoveAll(path.Join(utils.HYPER_ROOT, "services", p.Id))
	os.RemoveAll(path.Join(utils.HYPER_ROOT, "hosts", p.Id))

	daemon.RemovePodContainer(p)
	daemon.DeleteVolumeId(p.Id)
	daemon.db.DeletePod(p.Id)
}

func (daemon *Daemon) RemovePodContainer(p *LegacyPod) {
	for _, c := range p.PodStatus.Containers {
		glog.V(1).Infof("Ready to rm container: %s", c.Id)
		if err := daemon.Daemon.ContainerRm(c.Id, &dockertypes.ContainerRmConfig{}); err != nil {
			glog.Warningf("Error to rm container: %s", err.Error())
		}
	}
	daemon.db.DeleteP2C(p.Id)
}
