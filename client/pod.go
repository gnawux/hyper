package client

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/hyperhq/hyper/engine"
	"github.com/hyperhq/runv/hypervisor/types"

	gflag "github.com/jessevdk/go-flags"
)

func (cli *HyperClient) CreatePod(jsonbody string, remove bool) (string, error) {
	v := url.Values{}
	v.Set("podArgs", jsonbody)
	if remove {
		v.Set("remove", "yes")
	}
	body, statusCode, err := readBody(cli.call("POST", "/pod/create?"+v.Encode(), nil, nil))
	if statusCode == 404 {
		if err := cli.PullImages(jsonbody); err != nil {
			return "", fmt.Errorf("failed to pull images: %s", err.Error())
		}
		if body, _, err = readBody(cli.call("POST", "/pod/create?"+v.Encode(), nil, nil)); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	out := engine.NewOutput()
	remoteInfo, err := out.AddEnv()
	if err != nil {
		return "", err
	}

	if _, err := out.Write(body); err != nil {
		return "", fmt.Errorf("Error reading remote info: %s", err)
	}
	out.Close()
	errCode := remoteInfo.GetInt("Code")
	if errCode == types.E_OK {
		//fmt.Println("VM is successful to start!")
	} else {
		// case types.E_CONTEXT_INIT_FAIL:
		// case types.E_DEVICE_FAIL:
		// case types.E_QMP_INIT_FAIL:
		// case types.E_QMP_COMMAND_FAIL:
		if errCode != types.E_BAD_REQUEST &&
			errCode != types.E_FAILED {
			return "", fmt.Errorf("Error code is %d", errCode)
		} else {
			return "", fmt.Errorf("Cause is %s", remoteInfo.Get("Cause"))
		}
	}
	return remoteInfo.Get("ID"), nil
}

func (cli *HyperClient) HyperCmdStart(args ...string) error {
	var opts struct {
		// OnlyVm    bool     `long:"onlyvm" default:"false" value-name:"false" description:"Only start a new VM"`
		Cpu int `short:"c" long:"cpu" default:"1" value-name:"1" description:"CPU number for the VM"`
		Mem int `short:"m" long:"memory" default:"128" value-name:"128" description:"Memory size (MB) for the VM"`
	}
	var parser = gflag.NewParser(&opts, gflag.Default)
	parser.Usage = "start [-c 1 -m 128]| POD_ID \n\nLaunch a 'pending' pod"
	args, err := parser.Parse()
	if err != nil {
		if !strings.Contains(err.Error(), "Usage") {
			return err
		} else {
			return nil
		}
	}
	if false {
		// Only run a new VM
		v := url.Values{}
		v.Set("cpu", fmt.Sprintf("%d", opts.Cpu))
		v.Set("mem", fmt.Sprintf("%d", opts.Mem))
		body, _, err := readBody(cli.call("POST", "/vm/create?"+v.Encode(), nil, nil))
		if err != nil {
			return err
		}
		out := engine.NewOutput()
		remoteInfo, err := out.AddEnv()
		if err != nil {
			return err
		}

		if _, err := out.Write(body); err != nil {
			return fmt.Errorf("Error reading remote info: %s", err)
		}
		out.Close()
		errCode := remoteInfo.GetInt("Code")
		if errCode == types.E_OK {
			//fmt.Println("VM is successful to start!")
		} else {
			// case types.E_CONTEXT_INIT_FAIL:
			// case types.E_DEVICE_FAIL:
			// case types.E_QMP_INIT_FAIL:
			// case types.E_QMP_COMMAND_FAIL:
			if errCode != types.E_BAD_REQUEST &&
				errCode != types.E_FAILED {
				return fmt.Errorf("Error code is %d", errCode)
			} else {
				return fmt.Errorf("Cause is %s", remoteInfo.Get("Cause"))
			}
		}
		fmt.Printf("New VM id is %s\n", remoteInfo.Get("ID"))
		return nil
	}
	if len(args) < 2 {
		return fmt.Errorf("\"start\" requires a minimum of 1 argument, please provide POD ID.\n")
	}
	var (
		podId string
		vmId  string
	)
	if len(args) == 2 {
		podId = args[1]
	}
	if len(args) == 3 {
		podId = args[1]
		vmId = args[2]
	}
	// fmt.Printf("Pod ID is %s, VM ID is %s\n", podId, vmId)
	_, err = cli.StartPod(podId, vmId, false)
	if err != nil {
		return err
	}
	fmt.Fprintf(cli.out, "Successfully started the Pod(%s)\n", podId)
	return nil
}

func (cli *HyperClient) StartPod(podId, vmId string, tty bool) (string, error) {
	var tag string = ""
	v := url.Values{}
	v.Set("podId", podId)
	v.Set("vmId", vmId)

	if tty {
		tag = cli.GetTag()
	}
	v.Set("tag", tag)

	if !tty {
		return cli.startPodWithoutTty(&v)
	} else {
		return "", cli.hijackRequest("pod/start", podId, tag, &v)
	}
}

func (cli *HyperClient) startPodWithoutTty(v *url.Values) (string, error) {

	body, _, err := readBody(cli.call("POST", "/pod/start?"+v.Encode(), nil, nil))
	if err != nil {
		return "", err
	}
	out := engine.NewOutput()
	remoteInfo, err := out.AddEnv()
	if err != nil {
		return "", err
	}

	if _, err := out.Write(body); err != nil {
		return "", fmt.Errorf("Error reading remote info: %s", err)
	}
	out.Close()
	errCode := remoteInfo.GetInt("Code")
	if errCode != types.E_OK {
		if errCode != types.E_BAD_REQUEST &&
			errCode != types.E_FAILED {
			return "", fmt.Errorf("Error code is %d", errCode)
		} else {
			return "", fmt.Errorf("Cause is %s", remoteInfo.Get("Cause"))
		}
	}
	return remoteInfo.Get("ID"), nil
}
