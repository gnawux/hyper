package daemon

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/golang/glog"
	apitypes "github.com/hyperhq/hyperd/types"
	"github.com/hyperhq/hyperd/utils"
	"github.com/hyperhq/runv/hypervisor"
	"github.com/hyperhq/runv/hypervisor/types"
)

func (daemon *Daemon) CreateVm(cpu, mem int, async bool) (*hypervisor.Vm, error) {
	vm, err := daemon.StartVm("", cpu, mem, false)
	if err != nil {
		return nil, err
	}

	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			daemon.KillVm(vm.Id)
		}
	}()

	if !async {
		err = daemon.WaitVmStart(vm)
		if err != nil {
			return nil, err
		}
	}

	return vm, nil
}

func (daemon *Daemon) KillVm(vmId string) (int, string, error) {
	glog.V(3).Infof("KillVm %s", vmId)
	vm, ok := daemon.VmList.Get(vmId)
	if !ok {
		glog.V(3).Infof("Cannot find vm %s", vmId)
		return 0, "", nil
	}
	vm.Kill()
	daemon.RemoveVm(vmId)

	return 0, "", nil
}

func (p *LegacyPod) AssociateVm(daemon *Daemon, vmId string) error {
	if p.VM != nil && p.VM.Id != vmId {
		return fmt.Errorf("pod %s already has vm %s, but trying to associate with %s", p.Id, p.VM.Id, vmId)
	} else if p.VM != nil {
		return nil
	}

	vmData, err := daemon.db.GetVM(vmId)
	if err != nil {
		return err
	}
	glog.V(1).Infof("Get data for vm(%s) pod(%s)", vmId, p.Id)

	p.VM = daemon.VmList.NewVm(vmId, p.Spec.Resource.Vcpu, p.Spec.Resource.Memory, false)
	p.PodStatus.Vm = vmId

	err = p.VM.AssociateVm(p.PodStatus, vmData)
	if err != nil {
		p.VM = nil
		p.PodStatus.Vm = ""
		return err
	}

	daemon.VmList.Add(p.VM)

	err = p.startLogging(daemon)
	if err != nil {
		glog.Errorf("fail to start logging for pod %s: %v", p.Id, err)
	}

	return nil
}

func (daemon *Daemon) ReleaseAllVms() (int, error) {
	var (
		ret       = types.E_OK
		err error = nil
	)

	daemon.VmList.Foreach(func(vm *hypervisor.Vm) error {
		glog.V(3).Infof("release vm %s", vm.Id)
		ret, err = vm.ReleaseVm()
		if err != nil {
			// FIXME: return nil to continue to release other vms?
			glog.Errorf("fail to release vm %s: %v", vm.Id, err)
			return err
		}
		delete(daemon.VmList.vms, vm.Id)
		return nil
	})

	return ret, err
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
