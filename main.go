// olliesrv - 9P server for ollie sessions
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"9fans.net/go/plan9/client"
	p9 "olliesrv/internal/p9"
)

const serviceName = "ollie"

var mountPath = flag.String("mount", "", "FUSE mount path (default: $HOME/mnt/ollie)")

func main() {
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: olliesrv <start|fgstart|stop|status>")
		os.Exit(1)
	}

	ns := client.Namespace()
	if ns == "" {
		fmt.Fprintln(os.Stderr, "no namespace")
		os.Exit(1)
	}

	sockPath := filepath.Join(ns, serviceName)
	pidPath := filepath.Join(ns, serviceName+".pid")

	switch flag.Arg(0) {
	case "start":
		if isRunning(sockPath) {
			fmt.Println("olliesrv already running")
			os.Exit(0)
		}
		daemonize(pidPath)
	case "fgstart":
		if isRunning(sockPath) {
			fmt.Println("olliesrv already running")
			os.Exit(0)
		}
		runServer(sockPath, pidPath)
	case "stop":
		stopServer(sockPath, pidPath)
	case "status":
		if isRunning(sockPath) {
			fmt.Println("olliesrv running")
		} else {
			fmt.Println("olliesrv not running")
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: olliesrv <start|fgstart|stop|status>")
		os.Exit(1)
	}
}

func isRunning(sockPath string) bool {
	conn, err := net.Dial("unix", sockPath)
	if err == nil {
		conn.Close()
		return true
	}
	return false
}

func daemonize(pidPath string) {
	exe, _ := os.Executable()
	args := []string{"fgstart"}
	if *mountPath != "" {
		args = append(args, "-mount", *mountPath)
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("olliesrv started (pid %d)\n", cmd.Process.Pid)
}

func stopServer(sockPath, pidPath string) {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		fmt.Println("olliesrv not running")
		return
	}
	var pid int
	fmt.Sscanf(string(data), "%d", &pid)
	if pid > 0 {
		syscall.Kill(pid, syscall.SIGTERM) //nolint:errcheck
	}
	os.Remove(sockPath) //nolint:errcheck
	os.Remove(pidPath)  //nolint:errcheck
	fmt.Println("olliesrv stopped")
}

func runServer(sockPath, pidPath string) {
	// Remove stale socket
	if _, err := os.Stat(sockPath); err == nil {
		os.Remove(sockPath) //nolint:errcheck
	}

	// Write PID file
	os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644) //nolint:errcheck

	srv := p9.New()

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go srv.Serve(conn)
		}
	}()

	fmt.Printf("olliesrv listening on %s\n", sockPath)

	// Optional FUSE mount
	mnt := *mountPath
	if mnt == "" {
		mnt = os.Getenv("OLLIE_9MOUNT")
	}
	if mnt == "" {
		home, _ := os.UserHomeDir()
		mnt = filepath.Join(home, "mnt", "ollie")
	}
	var fuseCmd *exec.Cmd
	if err := os.MkdirAll(mnt, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot create mount dir: %v\n", err)
	} else {
		fuseCmd = exec.Command("9pfuse", sockPath, mnt)
		if err := fuseCmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: 9pfuse: %v\n", err)
			fuseCmd = nil
		} else {
			fmt.Printf("mounted at %s\n", mnt)
		}
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("shutting down")
	if fuseCmd != nil {
		exec.Command("fusermount", "-u", mnt).Run() //nolint:errcheck
		fuseCmd.Wait()                               //nolint:errcheck
	}
	listener.Close() //nolint:errcheck
	os.Remove(sockPath)
	os.Remove(pidPath)
}
