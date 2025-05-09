// Copyright (c) Contributors to the Apptainer project, established as
//   Apptainer a Series of LF Projects LLC.
//   For website terms of use, trademark policy, privacy policy and other
//   project policies see https://lfprojects.org/policies
// Copyright (c) 2018-2020, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package starter

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/apptainer/apptainer/internal/pkg/metric"
	"github.com/apptainer/apptainer/internal/pkg/runtime/engine"
	"github.com/apptainer/apptainer/internal/pkg/util/mainthread"
	signalutil "github.com/apptainer/apptainer/internal/pkg/util/signal"
	"github.com/apptainer/apptainer/pkg/sylog"
	"github.com/apptainer/apptainer/pkg/util/apptainerconf"
)

func createContainer(ctx context.Context, rpcSocket int, containerPid int, e *engine.Engine, fatalChan chan error) {
	comm := os.NewFile(uintptr(rpcSocket), "rpc-socket")
	if comm == nil {
		fatalChan <- fmt.Errorf("bad RPC socket file descriptor")
		return
	}
	rpcConn, err := net.FileConn(comm)
	comm.Close()
	if err != nil {
		fatalChan <- fmt.Errorf("failed to copy unix socket descriptor: %s", err)
		return
	}

	configFile := apptainerconf.GetCurrentConfig()
	if configFile != nil && configFile.AllowMonitoring {
		apptheusMetric, err := metric.New()
		if err != nil {
			sylog.Debugf("Failed to establish connection to apptheus, err: %s", err)
		}
		e.Common.ApptheusSocket = apptheusMetric
	}

	err = e.CreateContainer(ctx, containerPid, rpcConn)
	if err != nil {
		fatalChan <- fmt.Errorf("container creation failed: %s", err)
		return
	}

	rpcConn.Close()
}

func startContainer(ctx context.Context, masterSocket int, containerPid int, e *engine.Engine, fatalChan chan error) {
	comm := os.NewFile(uintptr(masterSocket), "master-socket")
	if comm == nil {
		fatalChan <- fmt.Errorf("bad master socket file descriptor")
		return
	}
	conn, err := net.FileConn(comm)
	comm.Close()
	if err != nil {
		fatalChan <- fmt.Errorf("failed to create master connection: %s", err)
		return
	}
	defer conn.Close()

	data := make([]byte, 1)

	// special path for engines which needs to stop before executing
	// container process
	if obj, ok := e.Operations.(interface {
		PreStartProcess(context.Context, int, net.Conn, chan error) error
	}); ok {
		_, err := conn.Read(data)
		if err != nil {
			if err != io.EOF {
				fatalChan <- fmt.Errorf("error while reading master socket data: %s", err)
				return
			}
			// EOF means something goes wrong in stage 2, don't send error via
			// fatalChan, error will be reported by stage 2 and the process
			// status will be set accordingly via MonitorContainer method below
			sylog.Debugf("stage 2 process was interrupted, waiting status")
			return
		} else if data[0] == 'f' {
			// StartProcess reported an error in stage 2, don't send error via
			// fatalChan, error will be reported by stage 2 and the process
			// status will be set accordingly via MonitorContainer method below
			sylog.Debugf("stage 2 process reported an error, waiting status")
			return
		}
		if err := obj.PreStartProcess(ctx, containerPid, conn, fatalChan); err != nil {
			fatalChan <- fmt.Errorf("pre start process failed: %s", err)
			return
		}
	}
	// wait container process execution, EOF means container process
	// was executed and master socket was closed by stage 2. If data
	// byte sent is equal to 'f', it means an error occurred in
	// StartProcess, just return by waiting error and process status
	_, err = conn.Read(data)
	if (err != nil && err != io.EOF) || data[0] == 'f' {
		sylog.Debugf("stage 2 process reported an error, waiting status")
		return
	}

	err = e.PostStartProcess(ctx, containerPid)
	if err != nil {
		fatalChan <- fmt.Errorf("post start process failed: %s", err)
		return
	}
}

// Master initializes a runtime engine and runs it.
//
// Saved uid 0 is preserved when run with suid flow, so that
// the master is capable to escalate its privileges to setup
// container environment properly.
func Master(rpcSocket, masterSocket int, containerPid int, e *engine.Engine) {
	var status syscall.WaitStatus
	fatalChan := make(chan error, 1)

	// we could receive signal from child with CreateContainer call so we
	// set the signal handler earlier to queue signals until MonitorContainer
	// is called to handle them
	// Use a channel size of two here, since we may receive SIGURG, which is
	// used for non-cooperative goroutine preemption starting with Go 1.14.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals)

	ctx := context.TODO()

	go createContainer(ctx, rpcSocket, containerPid, e, fatalChan)

	go startContainer(ctx, masterSocket, containerPid, e, fatalChan)

	go func() {
		var err error
		status, err = e.MonitorContainer(containerPid, signals)
		fatalChan <- err
	}()

	fatal := <-fatalChan

	if err := e.CleanupContainer(ctx, fatal, status); err != nil {
		sylog.Errorf("container cleanup failed: %s", err)
	}

	if fatal != nil {
		sylog.Fatalf("%s", fatal)
	}

	// reset signal handlers
	signal.Reset()

	exitCode := 0

	if status.Signaled() {
		s := status.Signal()
		sylog.Debugf("Child exited due to signal %d", s)
		exitCode = 128 + int(s)

		// mimic signal
		mainthread.Execute(func() {
			signalutil.Raise(s)
		})
	} else if status.Exited() {
		sylog.Debugf("Child exited with exit status %d", status.ExitStatus())
		exitCode = status.ExitStatus()
	}

	// if previous signal didn't interrupt process
	os.Exit(exitCode)
}
