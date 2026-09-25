package main

// Kill a function instance from outside the cluster: the host's kubeconfig and a shell-out, no RBAC.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// In the atomic variant the interceptor is PID 1 and must survive: kill its node child.
// Elsewhere node IS PID 1 and cannot be signalled from inside, so the runtime stops it.
func killMethod(variant string) string {
	if variant == "atomic" {
		return "exec-kill"
	}
	return "crictl-stop"
}

func sh(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// The host is the node, so crictl runs on it.
func crictl(args ...string) (string, error) {
	return sh("sudo", append([]string{"crictl"}, args...)...)
}

// Every Ready pod is killed: the ingress may route to any of them.
func readyPods(service string) []string {
	tmpl := `{range .items[?(@.status.phase=="Running")]}{.metadata.name}{"\t"}` +
		`{.metadata.deletionTimestamp}{"\t"}{.status.containerStatuses[*].ready}{"\n"}{end}`
	out, err := sh("kubectl", "get", "pod", "-l",
		"serving.knative.dev/service="+service, "-o", "jsonpath="+tmpl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  crash: cannot list pods for %s: %v\n", service, err)
		return nil
	}
	var pods []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 || f[0] == "" || f[1] != "" || strings.Contains(f[2], "false") {
			continue
		}
		pods = append(pods, f[0])
	}
	return pods
}

func (h *loadgen) kill(service, method string) {
	for _, pod := range readyPods(service) {
		var err error
		switch method {
		case "crictl-stop":
			var cid string
			cid, err = crictl("ps", "-q",
				"--name", "user-container", "--label", "io.kubernetes.pod.name="+pod)
			if err == nil && cid != "" {
				_, err = crictl("stop", "--timeout=0", strings.Split(cid, "\n")[0])
			}
		default:
			_, err = sh("kubectl", "exec", pod, "-c", "user-container", "--",
				"sh", "-c", "kill -9 $(pgrep node)")
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  crash: %s on %s failed: %v\n", method, pod, err)
		}
	}
}
