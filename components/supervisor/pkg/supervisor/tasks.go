// Copyright (c) 2020 TypeFox GmbH. All rights reserved.
// Licensed under the GNU Affero General Public License (AGPL).
// See License-AGPL.txt in the project root for license information.

package supervisor

import (
	"bufio"
	"context"
	"io"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gitpod-io/gitpod/common-go/log"
	csapi "github.com/gitpod-io/gitpod/content-service/api"
	"github.com/gitpod-io/gitpod/supervisor/api"
	"github.com/gitpod-io/gitpod/supervisor/pkg/backup"
	"github.com/gitpod-io/gitpod/supervisor/pkg/terminal"
)

type runContext struct {
	contentSource csapi.WorkspaceInitSource
	headless      bool
	prebuildChans []*chan bool
}

type tasksManager struct {
	config            *Config
	tasks             map[string]*api.TasksStatus
	ready             chan struct{}
	terminalService   *terminal.MuxTerminalService
	inWorkspaceHelper *backup.InWorkspaceHelper
}

func newTasksManager(config *Config, terminalService *terminal.MuxTerminalService, inWorkspaceHelper *backup.InWorkspaceHelper) *tasksManager {
	return &tasksManager{
		config:            config,
		terminalService:   terminalService,
		inWorkspaceHelper: inWorkspaceHelper,
		tasks:             make(map[string]*api.TasksStatus),
		ready:             make(chan struct{}),
	}
}

func (tm *tasksManager) Run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	defer close(tm.ready)

	tasks, err := tm.config.getGitpodTasks()
	if err != nil {
		log.WithError(err).Fatal()
		return
	}
	if tasks == nil {
		log.Info("no gitpod tasks found")
		return
	}

	select {
	case <-ctx.Done():
		return
	case <-tm.inWorkspaceHelper.ContentReady():
	}

	contentSource, _ := tm.inWorkspaceHelper.ContentSource()
	headless := tm.config.GitpodHeadless != nil && *tm.config.GitpodHeadless == "true"
	runContext := &runContext{
		contentSource: contentSource,
		headless:      headless,
	}
	if headless {
		runContext.prebuildChans = make([]*chan bool, len(*tasks))
	}
	for i, task := range *tasks {
		command := task.getCommand(i, runContext)
		if command == "" {
			continue
		}

		taskLog := log.WithField("command", command)
		taskLog.Info("starting a task terminal...")
		openRequest := &api.OpenTerminalRequest{}
		if task.Env != nil {
			openRequest.Env = *task.Env
		} else {
			openRequest.Env = make(map[string]string)
		}
		resp, err := tm.terminalService.Open(ctx, openRequest)
		if err != nil {
			taskLog.WithError(err).Fatal("cannot open new task terminal")
			continue
		}
		alias := resp.Alias
		taskLog = taskLog.WithField("alias", alias)

		if headless {
			terminal, ok := tm.terminalService.Mux.Get(alias)
			if !ok {
				taskLog.WithError(err).Fatal("cannot find a task terminal")
				err = tm.terminalService.Mux.Close(alias)
				if err != nil {
					taskLog.WithError(err).Fatal("cannot close a task terminal")
				}
				continue
			}
			runContext.watch(i, terminal)
		}

		_, err = tm.terminalService.Write(ctx, &api.WriteTerminalRequest{
			Alias: alias,
			Stdin: []byte(command + "\n"),
		})
		if err != nil {
			taskLog.WithError(err).Fatal("cannot send a commant to a task terminal")
			err = tm.terminalService.Mux.Close(alias)
			if err != nil {
				taskLog.WithError(err).Fatal("cannot close a task terminal")
			}
			continue
		}

		taskStatus := api.TasksStatus{
			Alias: alias,
		}
		if task.Name != nil {
			taskStatus.Name = *task.Name
		} else {
			taskStatus.Name = tm.terminalService.DefaultWorkdir
		}
		if task.OpenIn != nil {
			taskStatus.Name = *task.OpenIn
		}
		if task.OpenMode != nil {
			taskStatus.Name = *task.OpenMode
		}
		tm.tasks[alias] = &taskStatus
		taskLog.Info("task terminal has been started")
	}

	if headless {
		runContext.report(ctx)
	}
}

func (task *TaskConfig) getCommand(i int, context *runContext) string {
	taskCommands := task.getCommands(i, context)
	command := reduce(taskCommands, func(composed string, command string) string {
		if composed != "" {
			composed += " && "
		}
		return composed + "{\n" + command + "\n}"
	})

	if context.headless {
		// it's important that prebuild tasks exit eventually
		// also, we need to save the log output in the workspace
		if strings.TrimSpace(command) == "" {
			return "exit"
		}
		return command + "; exit"
	}
	if strings.TrimSpace(command) == "" {
		return ""
	}
	histfile := "/workspace/.gitpod/cmd-" + strconv.Itoa(i)
	histfileCommands := taskCommands
	if context.contentSource == csapi.WorkspaceInitFromPrebuild {
		histfileCommands = []*string{task.Before, task.Init, task.Prebuild, task.Command}
	}
	err := ioutil.WriteFile(histfile, []byte(reduce(histfileCommands, func(composed string, command string) string {
		return composed + command + "\r\n"
	})), 0644)
	if err != nil {
		log.WithField("histfile", histfile).WithError(err).Fatal("cannot write histfile")
		return command
	}
	// the space at beginning of the HISTFILE command prevents the HISTFILE command itself from appearing in
	// the bash history.
	return " HISTFILE=" + histfile + " history -r; " + command
}

func (task *TaskConfig) getCommands(i int, context *runContext) []*string {
	if context.headless {
		// prebuild
		return []*string{task.Before, task.Init, task.Prebuild}
	}
	if context.contentSource == csapi.WorkspaceInitFromPrebuild {
		// prebuilt
		prebuildLogFileName := prebuildLogFileName(i)
		legacyPrebuildLogFileName := "/workspace/.prebuild-log-" + strconv.Itoa(i)
		printlogs := "[ -r " + legacyPrebuildLogFileName + " ] && cat " + legacyPrebuildLogFileName + "; [ -r " + prebuildLogFileName + " ] && cat " + prebuildLogFileName + "; true"
		return []*string{task.Before, &printlogs, task.Command}
	}
	if context.contentSource == csapi.WorkspaceInitFromBackup {
		// restart
		return []*string{task.Before, task.Command}
	}
	// init
	return []*string{task.Before, task.Init, task.Command}

}

func prebuildLogFileName(i int) string {
	return "/workspace/.gitpod/prebuild-log-" + strconv.Itoa(i)
}

func (context *runContext) watch(i int, terminal *terminal.Term) {
	stdout := terminal.Stdout.Listen()
	start := time.Now()
	prebuildChan := make(chan bool)
	context.prebuildChans[i] = &prebuildChan
	go func() {
		fileName := prebuildLogFileName(i)
		file, err := os.Create(fileName)
		if err != nil {
			log.WithError(err).Fatal("cannot create a prebuild log file")
			prebuildChan <- false
			return
		}
		defer file.Close()
		fileWriter := bufio.NewWriter(file)

		log.Info("Writing build output to " + fileName)

		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if err == io.EOF {
				elapsed := time.Since(start)
				duration := ""
				if elapsed >= 1*time.Minute {
					elapsedInMinutes := strconv.Itoa(int(elapsed.Minutes()))
					duration = "🎉 You just saved " + elapsedInMinutes + " minute"
					if elapsedInMinutes != "1" {
						duration += "s"
					}
					duration += " of watching your code build.\n"
				}
				fileWriter.Write(buf[:n])
				fileWriter.WriteString("\n🍌 This task ran as part of a workspace prebuild.\n" + duration + "\n")
				fileWriter.Flush()
				prebuildChan <- true
				break
			}
			if err != nil {
				log.WithError(err).Fatal("cannot read from a task terminal")
				prebuildChan <- false
				return
			}
			fileWriter.Write(buf[:n])
			log.WithField("type", "workspaceTaskOutput").WithField("data", string(buf[:n])).Info()
		}
	}()
}

func (context *runContext) report(ctx context.Context) {
	ok := true
	for _, prebuildChan := range context.prebuildChans {
		if prebuildChan != nil {
			select {
			case <-ctx.Done():
				return
			case prebuildOk := <-*prebuildChan:
				if !prebuildOk {
					ok = false
				}
			}
		}
	}
	log.WithField("type", "workspaceTaskOutput").WithField("data", "🚛 uploading prebuilt workspace").Info()
	if ok {
		log.WithField("type", "workspaceTaskDone").Info()
	} else {
		log.WithField("type", "workspaceTaskFailed").WithField("error", "one of the tasks failed with non-zero exit code").Info()
	}
}

func reduce(commands []*string, reducer func(string, string) string) string {
	composed := ""
	for _, command := range commands {
		if command != nil {
			if strings.TrimSpace(*command) != "" {
				composed = reducer(composed, *command)
			}
		}
	}
	return composed
}
