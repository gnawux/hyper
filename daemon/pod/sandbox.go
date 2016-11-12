package pod

import (
	"time"

	"github.com/hyperhq/hyperd/lib/hlog"
	"github.com/hyperhq/runv/hypervisor"
	runvtypes "github.com/hyperhq/runv/hypervisor/types"
)

const (
	maxReleaseRetry = 3
)

func dissociateSandbox(sandbox *hypervisor.Vm, retry int) error {
	if sandbox == nil {
		return nil
	}

	rval, err := sandbox.ReleaseVm()
	if err != nil {
		hlog.Log(WARNING, "SB[%s] failed to release sandbox: %v", sandbox.Id, err)
		if rval == runvtypes.E_BUSY && retry < maxReleaseRetry{
			retry++
			hlog.Log(DEBUG, "SB[%s] retry release %d", sandbox.Id, retry)
			time.AfterFunc(100*time.Millisecond, func(){
				dissociateSandbox(sandbox, retry)
			})
			return nil
		}
		hlog.Log(INFO, "SB[%s] shutdown because of failed release", sandbox.Id)
		sandbox.Kill()
		return err
	}
	return nil
}