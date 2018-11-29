package driver

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/consul-template/signals"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/nomad/client/config"
	dstructs "github.com/hashicorp/nomad/client/driver/structs"
	"github.com/hashicorp/nomad/drivers/shared/executor"
	"github.com/hashicorp/nomad/helper/discover"
	"github.com/hashicorp/nomad/nomad/structs"
	pexecutor "github.com/hashicorp/nomad/plugins/executor"
)

// cgroupsMounted returns true if the cgroups are mounted on a system otherwise
// returns false
func cgroupsMounted(node *structs.Node) bool {
	_, ok := node.Attributes["unique.cgroup.mountpoint"]
	return ok
}

// createExecutor launches an executor plugin and returns an instance of the
// Executor interface
func createExecutor(w io.Writer, clientConfig *config.Config,
	executorConfig *pexecutor.ExecutorConfig) (executor.Executor, *plugin.Client, error) {

	c, err := json.Marshal(executorConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create executor config: %v", err)
	}
	bin, err := discover.NomadExecutable()
	if err != nil {
		return nil, nil, fmt.Errorf("unable to find the nomad binary: %v", err)
	}

	config := &plugin.ClientConfig{
		Cmd: exec.Command(bin, "executor", string(c)),
	}
	config.HandshakeConfig = pexecutor.HandshakeConfig
	config.Plugins = pexecutor.GetPluginMap(w, hclog.LevelFromString(clientConfig.LogLevel), executorConfig.FSIsolation)
	config.MaxPort = clientConfig.ClientMaxPort
	config.MinPort = clientConfig.ClientMinPort

	// setting the setsid of the plugin process so that it doesn't get signals sent to
	// the nomad client.
	if config.Cmd != nil {
		isolateCommand(config.Cmd)
	}

	executorClient := plugin.NewClient(config)
	rpcClient, err := executorClient.Client()
	if err != nil {
		return nil, nil, fmt.Errorf("error creating rpc client for executor plugin: %v", err)
	}

	raw, err := rpcClient.Dispense("executor")
	if err != nil {
		return nil, nil, fmt.Errorf("unable to dispense the executor plugin: %v", err)
	}
	executorPlugin := raw.(executor.Executor)
	return executorPlugin, executorClient, nil
}

func createExecutorWithConfig(config *plugin.ClientConfig, w io.Writer) (executor.Executor, *plugin.Client, error) {
	config.HandshakeConfig = pexecutor.HandshakeConfig

	// Setting this to DEBUG since the log level at the executor server process
	// is already set, and this effects only the executor client.
	config.Plugins = pexecutor.GetPluginMap(w, hclog.Debug, false)

	executorClient := plugin.NewClient(config)
	rpcClient, err := executorClient.Client()
	if err != nil {
		return nil, nil, fmt.Errorf("error creating rpc client for executor plugin: %v", err)
	}

	raw, err := rpcClient.Dispense("executor")
	if err != nil {
		return nil, nil, fmt.Errorf("unable to dispense the executor plugin: %v", err)
	}
	executorPlugin, ok := raw.(*pexecutor.ExecutorRPC)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected executor rpc type: %T", raw)
	}
	return executorPlugin, executorClient, nil
}

// killProcess kills a process with the given pid
func killProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// destroyPlugin kills the plugin with the given pid and also kills the user
// process
func destroyPlugin(pluginPid int, userPid int) error {
	var merr error
	if err := killProcess(pluginPid); err != nil {
		merr = multierror.Append(merr, err)
	}

	if err := killProcess(userPid); err != nil {
		merr = multierror.Append(merr, err)
	}
	return merr
}

// validateCommand validates that the command only has a single value and
// returns a user friendly error message telling them to use the passed
// argField.
func validateCommand(command, argField string) error {
	trimmed := strings.TrimSpace(command)
	if len(trimmed) == 0 {
		return fmt.Errorf("command empty: %q", command)
	}

	if len(trimmed) != len(command) {
		return fmt.Errorf("command contains extra white space: %q", command)
	}

	return nil
}

// GetKillTimeout returns the kill timeout to use given the tasks desired kill
// timeout and the operator configured max kill timeout.
func GetKillTimeout(desired, max time.Duration) time.Duration {
	maxNanos := max.Nanoseconds()
	desiredNanos := desired.Nanoseconds()

	// Make the minimum time between signal and kill, 1 second.
	if desiredNanos <= 0 {
		desiredNanos = (1 * time.Second).Nanoseconds()
	}

	// Protect against max not being set properly.
	if maxNanos <= 0 {
		maxNanos = (10 * time.Second).Nanoseconds()
	}

	if desiredNanos < maxNanos {
		return time.Duration(desiredNanos)
	}

	return max
}

// GetAbsolutePath returns the absolute path of the passed binary by resolving
// it in the path and following symlinks.
func GetAbsolutePath(bin string) (string, error) {
	lp, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("failed to resolve path to %q executable: %v", bin, err)
	}

	return filepath.EvalSymlinks(lp)
}

// getExecutorUser returns the user of the task, defaulting to
// dstructs.DefaultUnprivilegedUser if none was given.
func getExecutorUser(task *structs.Task) string {
	if task.User == "" {
		return dstructs.DefaultUnprivilegedUser
	}
	return task.User
}

// getTaskKillSignal looks up the signal specified for the task if it has been
// specified. If it is not supported on the platform, returns an error.
func getTaskKillSignal(signal string) (os.Signal, error) {
	if signal == "" {
		return os.Interrupt, nil
	}

	taskKillSignal := signals.SignalLookup[signal]
	if taskKillSignal == nil {
		return nil, fmt.Errorf("Signal %s is not supported", signal)
	}

	return taskKillSignal, nil
}
