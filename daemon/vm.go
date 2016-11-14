package daemon

import (
	"github.com/hyperhq/hyperd/daemon/pod"
)

func (daemon *Daemon) ReleaseAllVms() error {
	var (
		err error = nil
	)

	err = daemon.PodList.Foreach(func(p *pod.XPod) error {
		return p.Dissociate()
	})

	return err
}
