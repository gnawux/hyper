package daemon

import (
	"fmt"
	"io"

	"github.com/docker/docker/pkg/stdcopy"
	"github.com/golang/glog"

	"github.com/hyperhq/hyperd/daemon/pod"
	apitypes "github.com/hyperhq/hyperd/types"
	"github.com/hyperhq/hyperd/utils"
	"github.com/hyperhq/runv/hypervisor"
	"github.com/hyperhq/runv/hypervisor/types"
)

func (daemon *Daemon) CreatePod(podId string, podSpec *apitypes.UserPod) (*pod.Pod, error) {
	//FIXME: why restrict to 1024
	if daemon.PodList.CountRunning() >= 1024 {
		return nil, fmt.Errorf("There have already been %d running Pods", 1024)
	}
	if podId == "" {
		podId = fmt.Sprintf("pod-%s", utils.RandStr(10, "alpha"))
	}

	if podSpec.Id == "" {
		podSpec = podId
	}

	if _, ok := daemon.PodList.Get(podSpec.Id); ok {
		return nil, fmt.Errorf("pod %s already exist", podSpec.Id)
	}

	if err := podSpec.Validate(); err != nil {
		return err
	}

	factory := pod.NewPodFactory(daemon.Storage, daemon, daemon.DefaultLog)
	sandbox, err := daemon.StartVm("", podSpec.Resource.Vcpu, podSpec.Resource.Memory, hypervisor.HDriver.SupportLazyMode())
	if err != nil {
		return nil, err
	}

	p := pod.NewPod(factory, sandbox, podSpec)

	confirm, err := daemon.PodList.PutPod(p)
	if err != nil {
		glog.Errorf("%s: failed to add pod: %v", p.Name, err)
		return nil, err
	}

	defer func() {
		close(confirm)
		if err != nil {
			//TODO: cleanup
		}
	}()

	err = p.CreatePod()
	if err != nil {
		glog.Errorf("failed to create pod %s", podSpec.Id)
		return nil, err
	}

	// TODO get persist info and store
	// confirm pod added
	confirm <- true
	return p, nil
}

//TODO: remove the tty stream in StartPod API, now we could support attach after created
func (daemon *Daemon) StartPod(stdin io.ReadCloser, stdout io.WriteCloser, podId, vmId string, attach bool) (int, string, error) {
	p, ok := daemon.PodList.Get(podId)
	if !ok {
		return -1, "", fmt.Errorf("The pod(%s) can not be found, please create it first", podId)
	}

	var waitTty chan bool

	if attach {
		glog.V(1).Info("Run pod with tty attached")

		ids := p.ContainerIdsOf(apitypes.UserContainer_REGULAR)
		for _, id := range ids {
			p.Attach( &hypervisor.TtyIO{
				Stdin:    stdin,
				Stdout:   stdout,
				Callback: make(chan *types.VmResponse, 1),
			}, id)

			waitTty = make(chan bool, 1)
			go func(){
				p.WaitContainerFinish(id, -1)
				waitTty <- true
			}()
			break
		}
	}

	glog.Infof("pod:%s, vm:%s", podId, vmId)

	success, failed, err := p.StartPod()
	if glog.V(1) && len(success) > 0 {
		glog.Info("%s, the following containers was started: %v", p.Name, success)
	}
	if len(failed) > 0 {
		glog.Errorf("%s, the following containers was failed to start: %v", p.Name, failed)
	}


	if waitTty != nil {
		<-waitTty
	}

	if err != nil && len(success) == 0 {
		return -1, err.Error(), err
	}

	return 0, "", err
}

func (daemon *Daemon) SetPodLabels(pn string, override bool, labels map[string]string) error {

	p, ok := daemon.PodList.Get(pn)
	if !ok {
		return fmt.Errorf("Can not get Pod %s info", pn)
	}

	err := p.SetLabel(labels, pn)
	if err != nil {
		return err
	}

	// TODO update the persisit info

	//spec, err := json.Marshal(p.spec)
	//if err != nil {
	//	return err
	//}
	//
	//if err := daemon.db.UpdatePod(p.name, spec); err != nil {
	//	return err
	//}

	return nil
}
