// Copyright (c) 2020 TypeFox GmbH. All rights reserved.
// Licensed under the GNU Affero General Public License (AGPL).
// See License-AGPL.txt in the project root for license information.

package uidmap

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	wsk8s "github.com/gitpod-io/gitpod/common-go/kubernetes"
	"github.com/gitpod-io/gitpod/common-go/log"
	ndeapi "github.com/gitpod-io/gitpod/ws-manager-node/api"
	"github.com/gitpod-io/gitpod/ws-manager-node/pkg/dispatch"
	"github.com/sirupsen/logrus"

	"golang.org/x/xerrors"
	"google.golang.org/grpc"
)

//
// BEWARE
// The code in this file, i.e. everything offered by WorkspaceBackupServer is accessible without further protection
// by user-reachable code. There's no server or ws-man in front of this interface. Keep this interface minimal, and
// be defensive!
//

const (
	// time between calls is the time that has to pass until we answer an RPC call again
	timeBetweenCalls = 10 * time.Second
)

// Uidmapper provides UID mapping services for creating Linux user namespaces
// from within a workspace.
type Uidmapper struct {
	// ProcLocation is the location of the node's proc filesystem
	ProcLocation string
}

// WorkspaceAdded is called when a new workspace is added
func (m *Uidmapper) WorkspaceAdded(ctx context.Context, ws *dispatch.Workspace) error {
	disp := dispatch.GetFromContext(ctx)
	if disp == nil {
		return xerrors.Errorf("no dispatch available")
	}

	host := wsk8s.WorkspaceSupervisorEndpoint(ws.WorkspaceID, disp.KubernetesNamespace)
	conn, err := grpc.DialContext(ctx, host, grpc.WithInsecure())
	if err != nil {
		return xerrors.Errorf("cannot dial workspace: %w", err)
	}

	iwh := ndeapi.NewInWorkspaceHelperClient(conn)

	var (
		canary         ndeapi.InWorkspaceHelper_UidmapCanaryClient
		requestCounter int
	)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if canary == nil {
			canary, err = iwh.UidmapCanary(ctx)
			if err != nil {
				log.WithError(err).WithFields(wsk8s.GetOWIFromObject(&ws.Pod.ObjectMeta)).Error("cannot connect uid mapper canary")
				time.Sleep(timeBetweenCalls)
				continue
			}
			log.WithFields(wsk8s.GetOWIFromObject(&ws.Pod.ObjectMeta)).Error("installed uid mapper canary")
		}

		msg, err := canary.Recv()
		if err != nil {
			return err
		}

		err = m.handleUIDMappingRequest(ctx, disp, ws, msg)
		if err != nil {
			log.WithError(err).WithFields(logrus.Fields{
				"containerID": ws.ContainerID,
				"workspaceId": ws.WorkspaceID,
				"instanceId":  ws.InstanceID,
			}).Error("cannot handle UID mapping request")
		}

		canary.Send(&ndeapi.UidmapCanaryResponse{})

		requestCounter++
		if requestCounter >= 2 {
			requestCounter = 0
			canary.CloseSend()

			canary = nil
			time.Sleep(timeBetweenCalls)
		}
	}
}

func (m *Uidmapper) handleUIDMappingRequest(ctx context.Context, disp *dispatch.Dispatch, ws *dispatch.Workspace, req *ndeapi.UidmapCanaryRequest) error {
	log.WithField("msg", fmt.Sprintf("%+v", req)).Info("received UID mapping request")

	containerPID, err := disp.CRI.ContainerPID(ctx, ws.ContainerID)
	if err != nil {
		return err
	}

	hostPID, err := m.findHostPID(uint64(containerPID), uint64(req.Pid))
	if err != nil {
		return err
	}
	log.WithField("hostPID", hostPID).WithField("inContainerPID", req.Pid).WithField("containerID", ws.ContainerID).Info("found host PID")

	return nil
}

// findHosPID translates an in-container PID to the root PID namespace.
func (m *Uidmapper) findHostPID(containerPID, inContainerPID uint64) (uint64, error) {
	paths := []string{filepath.Join(m.ProcLocation, fmt.Sprint(containerPID))}

	for {
		p := paths[0]
		paths = paths[1:]

		taskfn := filepath.Join(p, "task")
		tasks, err := ioutil.ReadDir(taskfn)
		if err != nil {
			continue
			// return 0, xerrors.Errorf("cannot read task file %s: %w", taskfn, err)
		}
		for _, task := range tasks {
			fn := filepath.Join(taskfn, task.Name(), "status")
			pid, nspid, err := readStatusFile(fn)
			log.WithFields(logrus.Fields{
				"pid":            pid,
				"nspid":          nspid,
				"err":            err,
				"fn":             fn,
				"containerPID":   containerPID,
				"inContainerPID": inContainerPID,
			}).Debug("reading status file")
			if err != nil {
				continue
				// return 0, err
			}
			for _, nsp := range nspid {
				if nsp == inContainerPID {
					return pid, nil
				}
			}

			paths = append(paths, filepath.Join(m.ProcLocation, fmt.Sprint(pid)))
		}

		if len(paths) == 0 {
			return 0, fmt.Errorf("cannot find in-container PID %d on the node", inContainerPID)
		}
	}
}

func readStatusFile(fn string) (pid uint64, nspid []uint64, err error) {
	f, err := os.Open(fn)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Pid:") {
			pid, err = strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "Pid:")), 10, 64)
			if err != nil {
				err = xerrors.Errorf("cannot parse pid in %s: %w", fn, err)
				return
			}
		}
		if strings.HasPrefix(line, "NSpid:") {
			fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "NSpid:")))
			for _, fld := range fields {
				pid, err = strconv.ParseUint(fld, 10, 64)
				if err != nil {
					err = xerrors.Errorf("cannot parse NSpid %v in %s: %w", fld, fn, err)
					return
				}

				nspid = append(nspid, pid)
			}
		}
	}
	if err = scanner.Err(); err != nil {
		return
	}

	return
}
