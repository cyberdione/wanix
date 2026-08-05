//go:build !js && !wasm

package headless

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"tractor.dev/wanix"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
)

// StartCommand allocates a normal Wanix auto task, binds its standard fds to
// the native process, and starts command through the registered task driver.
func StartCommand(root *wanix.Task, command string) (*wanix.Task, error) {
	newTask, err := fs.OpenContext(context.Background(), root.NS(), "#task/new/auto")
	if err != nil {
		return nil, err
	}
	id, err := io.ReadAll(newTask)
	_ = newTask.Close()
	if err != nil {
		return nil, err
	}
	taskID := strings.TrimSpace(string(id))
	task, err := root.Lookup(taskID)
	if err != nil {
		return nil, err
	}
	fds := fskit.MapFS{
		"0": fskit.FileFS(os.Stdin, "0"),
		"1": fskit.FileFS(os.Stdout, "1"),
		"2": fskit.FileFS(os.Stderr, "2"),
	}
	// The task-scoped API resolves #task/<id>/fd through the root task table;
	// retain the same binding in both views so GoJS syscall traffic and the
	// driver-side FD lookup observe one terminal capability.
	if err := root.NS().Bind(fds, ".", "#task/"+taskID+"/fd"); err != nil {
		return nil, fmt.Errorf("bind root task stdio: %w", err)
	}
	if err := task.NS().Bind(fds, ".", "#task/"+taskID+"/fd"); err != nil {
		return nil, fmt.Errorf("bind task stdio: %w", err)
	}
	if err := fs.WriteFile(root.NS(), "#task/"+taskID+"/cmd", []byte(command), 0644); err != nil {
		return nil, err
	}
	if err := fs.WriteFile(root.NS(), "#task/"+taskID+"/ctl", []byte("start"), 0644); err != nil {
		return nil, err
	}
	return task, nil
}

// WaitExit returns the task exit status after its registered driver records it.
func WaitExit(task *wanix.Task, interval time.Duration) (int, error) {
	if interval <= 0 {
		interval = 20 * time.Millisecond
	}
	path := "#task/" + task.ID() + "/exit"
	for {
		value, err := fs.ReadFile(task.NS(), path)
		if err == nil && strings.TrimSpace(string(value)) != "" {
			return strconv.Atoi(strings.TrimSpace(string(value)))
		}
		time.Sleep(interval)
	}
}
