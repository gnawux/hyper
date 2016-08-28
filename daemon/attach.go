package daemon

import (
	"fmt"
	"io"

	"github.com/docker/docker/pkg/stdcopy"
	"github.com/golang/glog"

	"github.com/hyperhq/runv/hypervisor"
	"github.com/hyperhq/runv/hypervisor/types"
)

func (daemon *Daemon) Attach(stdin io.ReadCloser, stdout io.WriteCloser, container string) error {
	var (
		err  error
	)

	tty := &hypervisor.TtyIO{
		Stdin:    stdin,
		Stdout:   stdout,
		Callback: make(chan *types.VmResponse, 1),
	}

	p, id, ok := daemon.PodList.GetByContainerIdOrName(container)
	if !ok {
		err = fmt.Errorf("cannot find container %s", container)
		glog.Error(err)
		return err
	}

	err = p.Attach(tty, id)
	if err != nil {
		return err
	}

	defer func() {
		glog.V(2).Info("Defer function for attach!")
	}()

	err = tty.WaitForFinish()

	return err
}
