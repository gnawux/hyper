package daemon

import (
	"fmt"
	"runtime"

	"github.com/golang/glog"
	"github.com/hyperhq/hyperd/daemon/pod"
	"github.com/hyperhq/runv/hypervisor"
	"github.com/hyperhq/runv/hypervisor/types"
)

func (daemon *Daemon) ReleaseAllVms() error {
	var (
		err error = nil
	)

	err = daemon.PodList.Foreach(func(p *pod.XPod) error {
		return p.Dissociate(0)
	})

	return err
}

func (daemon *Daemon) StartSandbox(cpu, mem int) (*hypervisor.Vm, error) {
	return daemon.StartVm("", cpu, mem, hypervisor.HDriver.SupportLazyMode())
}

func (daemon *Daemon) AssociateSandbox(id string) (*hypervisor.Vm, error) {
	vmData, err := daemon.db.GetVM(id)
	if err != nil {
		return err
	}
	vm := hypervisor.NewVm(id, 0, 0, false)
	err = vm.AssociateVm(vmData)
	if err != nil {
		return nil, err
	}
	return vm, nil
}

func (daemon *Daemon) StartVm(vmId string, cpu, mem int, lazy bool) (vm *hypervisor.Vm, err error) {
	var (
		DEFAULT_CPU = 1
		DEFAULT_MEM = 128
	)

	if cpu <= 0 {
		cpu = DEFAULT_CPU
	}
	if mem <= 0 {
		mem = DEFAULT_MEM
	}

	b := &hypervisor.BootConfig{
		CPU:    cpu,
		Memory: mem,
		Kernel: daemon.Kernel,
		Initrd: daemon.Initrd,
		Bios:   daemon.Bios,
		Cbfs:   daemon.Cbfs,
		Vbox:   daemon.VboxImage,
	}

	glog.V(1).Infof("The config: kernel=%s, initrd=%s", daemon.Kernel, daemon.Initrd)
	if vmId != "" || daemon.Bios != "" || daemon.Cbfs != "" || runtime.GOOS == "darwin" || lazy {
		vm, err = hypervisor.GetVm(vmId, b, false, lazy)
	} else {
		vm, err = daemon.Factory.GetVm(cpu, mem)
	}
	return vm, err
}

func (daemon *Daemon) WaitVmStart(vm *hypervisor.Vm) error {
	Status, err := vm.GetResponseChan()
	if err != nil {
		return err
	}
	defer vm.ReleaseResponseChan(Status)

	vmResponse := <-Status
	glog.V(1).Infof("Get the response from VM, VM id is %s, response code is %d!", vmResponse.VmId, vmResponse.Code)
	if vmResponse.Code != types.E_VM_RUNNING {
		return fmt.Errorf("Vbox does not start successfully")
	}
	return nil
}
