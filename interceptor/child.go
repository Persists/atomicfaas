package main

// The function runs as a child of the interceptor (PID 1); integration is env only.

import (
	"errors"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

const (
	functionPort = "8081"
	functionAddr = "127.0.0.1:" + functionPort
	proxyAddr    = "127.0.0.1:8082" // NB: 9090/9091 are taken by the stock queue-proxy
	shimPath     = "/atomic/shim.js"
)

func spawnFunction(argv []string) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	env := append(os.Environ(), "PORT="+functionPort, "ATOMIC_PROXY=http://"+proxyAddr)
	if _, err := os.Stat(shimPath); err == nil {
		env = append(env, "NODE_OPTIONS=--require "+shimPath)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		log.Fatalf("cannot start function %v: %v", argv, err)
	}
	return cmd
}

// SIGTERM is a drain and a clean exit of the function is passed on as exit 0, which the detector
// ignores; any other exit code is a crash.
func watchFunction(cmd *exec.Cmd) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		code := exitCode(err)
		log.Printf("function process exited (code %d); terminating container", code)
		os.Exit(code)
	case <-sig:
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		os.Exit(0)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() > 0 {
		return ee.ExitCode()
	}
	return 1
}

func waitForFunction(addr string) {
	for start := time.Now(); time.Since(start) < 30*time.Second; time.Sleep(100 * time.Millisecond) {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return
		}
	}
	log.Fatalf("function did not start listening on %s", addr)
}
