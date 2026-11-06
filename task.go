package wanix

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"tractor.dev/toolkit-go/engine/cli"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
	"tractor.dev/wanix/fs/memfs"
	"tractor.dev/wanix/fs/scratchfs"
	"tractor.dev/wanix/fs/vfs"
	"tractor.dev/wanix/misc"
	"tractor.dev/wanix/misc/shlex"
)

// contextKey is a value for use with context.WithValue. It's used as
// a pointer so it fits in an interface{} without allocation.
type contextKey struct {
	name string
}

func (k *contextKey) String() string { return "task context value " + k.name }

var (
	TaskContextKey = &contextKey{"task"}
)

func FromContext(ctx context.Context) (*Task, bool) {
	p, ok := ctx.Value(TaskContextKey).(*Task)
	return p, ok
}

type TaskDriver interface {
	Check(*Task) bool
	Start(*Task) error
}

type Task struct {
	driver                TaskDriver
	diagnosticProfile     string
	memoryControlScenario string
	parent                *Task
	ns                    *vfs.NS
	id                    int
	alias                 string
	kind                  string
	cmd                   string
	args                  []string
	env                   []string
	exit                  string
	dir                   string
	fds                   map[int]*openFile
	closedFDs             map[int]bool
	fdIdx                 int
	cleanup               []func()
	exited                bool
	cancel                context.CancelFunc
	fsys                  *TaskFS
	worker                any
	export                fs.FS
	w                     fs.FS
	mu                    sync.Mutex
	exitMu                sync.RWMutex // status snapshots must not reenter mu through FD routing
}

func Export(t *Task, export fs.FS) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.export = export
}

func SetWorker(t *Task, worker any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.worker = worker
}

func GetWorker(t *Task) any {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.worker
}

type openFile struct {
	file fs.File
	path string
	// more?
}

// NewRoot returns a task, so we dont really have the TaskFS
// in the public API. For now we have a couple methods that normally
// make more sense on TaskFS, but are on Task.

func (t *Task) Lookup(rid string) (*Task, error) {
	return t.fsys.Lookup(rid)
}

func (t *TaskFS) Lookup(rid string) (*Task, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tt, ok := t.resources[rid]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return tt.(*Task), nil
}

func (t *Task) Tasks() (tasks []*Task) {
	t.fsys.mu.Lock()
	defer t.fsys.mu.Unlock()
	for _, tt := range t.fsys.resources {
		tasks = append(tasks, tt.(*Task))
	}
	return tasks
}

// kludge: this would imply task specific registration, but its global.
// this is until we have a better registration system.
func (t *Task) Register(kind string, driver TaskDriver) {
	t.fsys.Register(kind, driver)
}

// AddCleanup registers resources owned by this task. Late registrations close immediately.
func (t *Task) AddCleanup(close func()) {
	t.mu.Lock()
	if t.exited {
		t.mu.Unlock()
		close()
		return
	}
	t.cleanup = append(t.cleanup, close)
	t.mu.Unlock()
}

// Exit cancels execution and releases owned resources exactly once.
func (t *Task) Exit(status string) {
	t.mu.Lock()
	if t.exited {
		t.mu.Unlock()
		return
	}
	t.exited = true
	t.exitMu.Lock()
	t.exit = status
	t.exitMu.Unlock()
	cleanup := t.cleanup
	t.cleanup = nil
	files := t.fds
	t.fds = make(map[int]*openFile)
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, close := range cleanup {
		close()
	}
	closed := make(map[fs.File]bool)
	for _, file := range files {
		if !closed[file.file] {
			closed[file.file] = true
			_ = file.file.Close()
		}
	}
}

func (t *Task) Start() error {
	if t.Context().Err() != nil {
		return fs.ErrClosed
	}
	if t.driver != nil {
		return t.driver.Start(t)
	}
	return nil
}

func (r *Task) ID() string {
	return strconv.Itoa(r.id)
}

func (r *Task) Context() context.Context {
	return r.NS().Context()
}

func (r *Task) Export() (fs.FS, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.export == nil {
		return nil, fs.ErrNotExist
	}
	return r.export, nil
}

func (r *Task) NS() *vfs.NS {
	return r.ns
}

func (r *Task) Parent() *Task {
	return r.parent
}

func (r *Task) Root() *Task {
	if r.parent == nil {
		return r
	}
	return r.parent.Root()
}

func (r *Task) Cmd() string {
	return r.cmd
}

func (r *Task) Arg(idx int) string {
	if idx < 0 || idx >= len(r.args) {
		return ""
	}
	return r.args[idx]
}

func (r *Task) Env() []string {
	return r.env
}

func (r *Task) Alias() string {
	return r.alias
}

func (r *Task) Dir() string {
	return r.dir
}

func (r *Task) Bind(srcPath, dstPath string, opts ...fs.BindOption) error {
	return r.ns.Bind(r.ns, srcPath, dstPath, opts...)
}

func (r *Task) Unbind(srcPath, dstPath string) error {
	return r.ns.Unbind(r.ns, srcPath, dstPath)
}

func (r *Task) OpenFD(file fs.File, path string) int {
	r.mu.Lock()
	if r.exited {
		r.mu.Unlock()
		_ = file.Close()
		return -1
	}
	defer r.mu.Unlock()
	r.fdIdx++
	r.fds[r.fdIdx] = &openFile{file: file, path: path}
	return r.fdIdx
}

func (r *Task) CloseFD(fd int) error {
	r.mu.Lock()
	f, ok := r.fds[fd]
	if !ok {
		r.mu.Unlock()
		return fs.ErrInvalid
	}
	delete(r.fds, fd)
	if fd >= 0 && fd < 3 {
		r.closedFDs[fd] = true
	}
	r.mu.Unlock()
	return f.file.Close()
}

func (r *Task) FD(fd int) (fs.File, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exited {
		return nil, "", fs.ErrClosed
	}
	if fd < 0 || fd > r.fdIdx {
		return nil, "", fs.ErrInvalid
	}
	if fd < 3 {
		if r.closedFDs[fd] {
			return nil, "", fs.ErrInvalid
		}
		if _, ok := r.fds[fd]; !ok {
			name := fmt.Sprintf("#task/%s/fd/%d", r.ID(), fd)
			// Resolve once, after namespace setup, then retain the exact file
			// until it is explicitly closed or the task exits.
			stdfile, err := r.NS().Open(name)
			if err != nil {
				return nil, "", err
			}
			r.fds[fd] = &openFile{file: stdfile, path: name}
		}
	}
	f, ok := r.fds[fd]
	if !ok {
		return nil, "", fs.ErrInvalid
	}
	return f.file, f.path, nil
}

func (r *Task) Open(name string) (fs.File, error) {
	return r.OpenContext(context.Background(), name)
}

func (r *Task) taskMap() fskit.MapFS {
	r.exitMu.RLock()
	exit := r.exit
	r.exitMu.RUnlock()
	m := fskit.MapFS{
		"ctl": misc.ControlFile(&cli.Command{
			Usage: "ctl",
			Short: "control the Task",
			Run: func(ctx *cli.Context, args []string) {
				// todo: cause fs error on error!
				if len(args) >= 3 && args[0] == "bind" {
					var (
						optstr string
					)
					flags := flag.NewFlagSet("bind", flag.ContinueOnError)
					flags.StringVar(&optstr, "o", "", "optional string flag")
					if err := flags.Parse(args[1:]); err != nil {
						log.Println(err)
						return
					}
					var opts []fs.BindOption
					for _, opt := range strings.Split(optstr, ",") {
						opts = append(opts, fs.BindOption(opt))
					}

					if err := r.Bind(flags.Arg(0), flags.Arg(1), opts...); err != nil {
						log.Println(err)
					}
					return
				}
				if len(args) == 3 && args[0] == "unbind" {
					if err := r.Unbind(args[1], args[2]); err != nil {
						log.Println(err)
					}
					return
				}
				if len(args) == 1 && args[0] == "stop" {
					go r.Exit("130")
					return
				}
				if len(args) == 1 && args[0] == "start" {
					if err := r.Start(); err != nil {
						log.Println(err)
					}
					return
				}
			},
		}),
		"id":   misc.FieldFile(r.ID()),
		"kind": misc.FieldFile(r.kind),
		"args": misc.FieldFile(strings.Join(r.args, "\n")),
		"cmd": misc.FieldFile(r.cmd, func(in []byte) error {
			if len(in) > 0 {
				r.cmd = strings.TrimSpace(string(in))
				args, err := shlex.Split(r.cmd, true)
				if err != nil {
					return err
				}
				r.args = args
			}
			return nil
		}),
		"alias": misc.FieldFile(r.alias, func(in []byte) error {
			if len(in) > 0 {
				oldalias := r.alias
				r.alias = strings.TrimSpace(string(in))
				r.fsys.mu.Lock()
				if oldalias != "" {
					delete(r.fsys.aliases, oldalias)
				}
				r.fsys.aliases[r.alias] = r
				r.fsys.mu.Unlock()
			}
			return nil
		}),
		"env": misc.FieldFile(strings.Join(r.env, "\n"), func(in []byte) error {
			if len(in) > 0 {
				r.env = strings.Split(strings.TrimSpace(string(in)), "\n")
			}
			return nil
		}),
		"dir": misc.FieldFile(r.dir, func(in []byte) error {
			if len(in) > 0 {
				r.dir = strings.TrimSpace(string(in))
			}
			return nil
		}),
		"exit": misc.FieldFile(exit, func(in []byte) error {
			if len(in) > 0 {
				status := strings.TrimSpace(string(in))
				go r.Exit(status)
			}
			return nil
		}),
		"binds": fskit.OpenFunc(func(ctx context.Context, name string) (fs.File, error) {
			return fskit.Entry("binds", 0555, []byte(r.NS().String()+"\n")).Open(name)
		}),
		"ns": r.ns,
		"w":  r.w,
	}
	if r.export != nil {
		m["export"] = r.export
	}
	return m
}

func (r *Task) Route(ctx context.Context, name string) (fs.FS, string, error) {
	return r.taskMap().Route(ctx, name)
}

func (r *Task) OpenContext(ctx context.Context, name string) (fs.File, error) {
	return fs.OpenContext(ctx, r.taskMap(), name)
}

type TaskFS struct {
	types     map[string]TaskDriver
	resources map[string]fs.FS
	aliases   map[string]fs.FS
	nextID    int
	mu        sync.Mutex
}

type autoDriver func(*Task) error

func (d autoDriver) Check(*Task) bool {
	return false
}

func (d autoDriver) Start(t *Task) error {
	return d(t)
}

func NewTaskFS() *TaskFS {
	d := &TaskFS{
		types:     make(map[string]TaskDriver),
		resources: make(map[string]fs.FS),
		aliases:   make(map[string]fs.FS),
		nextID:    0,
	}
	// empty namespace process
	d.Register("auto", autoDriver(func(t *Task) error {
		d.mu.Lock()
		drivers := make(map[string]TaskDriver, len(d.types))
		for kind, driver := range d.types {
			drivers[kind] = driver
		}
		d.mu.Unlock()
		// Drivers can resolve task routes or register other drivers. Never hold
		// the registry lock while invoking their callbacks.
		for kind, driver := range drivers {
			if driver.Check(t) {
				t.kind = kind
				return driver.Start(t)
			}
		}
		return nil
	}))
	return d
}

// linuxScratchSelector is an allocation profile, never a driver kind.
const linuxScratchSelector = "linux-scratch"
const linuxCapture2Selector = "linux-capture2-diagnostic"
const Capture2DiagnosticProfile = "capture-2-authored-layout-v1"

// DiagnosticProfile is fixed before publication and is never inherited.
func (t *Task) DiagnosticProfile() string     { return t.diagnosticProfile }
func (t *Task) MemoryControlScenario() string { return t.memoryControlScenario }

const MemoryControlProfile = "memory-controls-v1"

var memoryControlScenarios = [...]string{"rw", "none", "ro", "cross-none", "host-copy", "rw-store-readback", "none-load", "mprotect-hole-prefix"}

func ValidMemoryControlScenario(s string) bool {
	for _, name := range memoryControlScenarios {
		if s == name {
			return true
		}
	}
	return false
}
func memoryControlSelector(name string) string {
	for _, scenario := range memoryControlScenarios {
		if name == "linux-memory-controls-"+scenario {
			return scenario
		}
	}
	return ""
}

type TaskScratchPolicy uint8

const (
	LegacyScratch TaskScratchPolicy = iota
	LinuxRegularScratch
)

type TaskAllocOptions struct {
	Scratch           TaskScratchPolicy
	DiagnosticProfile string
}

func (d *TaskFS) Register(kind string, driver TaskDriver) {
	if kind == linuxScratchSelector || kind == linuxCapture2Selector || memoryControlSelector(kind) != "" {
		panic("wanix: linux-scratch is a reserved allocation selector")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.types[kind] = driver
}

func (d *TaskFS) Alloc(kind string, parent *Task) (*Task, error) {
	return d.AllocWithOptions(kind, parent, TaskAllocOptions{})
}

// AllocWithOptions chooses owned storage before publishing the task. The policy
// is not inherited and is never changed by executable detection, Start or Exit.
func (d *TaskFS) AllocWithOptions(kind string, parent *Task, opts TaskAllocOptions) (*Task, error) {
	return d.allocDerived(kind, parent, opts, "")
}
func (d *TaskFS) allocDerived(kind string, parent *Task, opts TaskAllocOptions, scenario string) (*Task, error) {
	if scenario != "" && (!ValidMemoryControlScenario(scenario) || kind != "linux" || opts.Scratch != LinuxRegularScratch || opts.DiagnosticProfile != "") {
		return nil, fs.ErrInvalid
	}
	if opts.DiagnosticProfile != "" && (opts.DiagnosticProfile != Capture2DiagnosticProfile || kind != "linux" || opts.Scratch != LinuxRegularScratch) {
		return nil, fs.ErrInvalid
	}
	if opts.Scratch != LegacyScratch && (opts.Scratch != LinuxRegularScratch || kind != "linux") {
		return nil, fs.ErrInvalid
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	driver, ok := d.types[kind]
	if !ok {
		return nil, fs.ErrNotExist
	}
	var writable fs.FS
	if opts.Scratch == LinuxRegularScratch {
		writable = scratchfs.New()
	} else {
		writable = memfs.New()
	}
	d.nextID++
	rid := strconv.Itoa(d.nextID)

	p := &Task{
		fsys:                  d,
		driver:                driver,
		diagnosticProfile:     opts.DiagnosticProfile,
		memoryControlScenario: scenario,
		id:                    d.nextID,
		kind:                  kind,
		fds:                   make(map[int]*openFile),
		closedFDs:             make(map[int]bool),
		fdIdx:                 3,
		w:                     writable,
	}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), TaskContextKey, p))
	p.cancel = cancel
	if parent != nil {
		p.parent = parent
		p.ns = parent.ns.Clone(ctx)
	} else {
		p.ns = vfs.New(ctx)
	}
	d.resources[rid] = p
	return p, nil
}

func (d *TaskFS) vfsNS(ctx context.Context) (*vfs.NS, error) {
	m := fskit.MapFS{
		"new": fskit.OpenFunc(func(ctx context.Context, name string) (fs.File, error) {
			if name == "." {
				var nodes []fs.DirEntry
				d.mu.Lock()
				for kind := range d.types {
					nodes = append(nodes, fskit.Entry(kind, 0555))
				}
				if _, ok := d.types["linux"]; ok {
					nodes = append(nodes, fskit.Entry(linuxScratchSelector, 0555), fskit.Entry(linuxCapture2Selector, 0555))
					for _, scenario := range memoryControlScenarios {
						nodes = append(nodes, fskit.Entry("linux-memory-controls-"+scenario, 0555))
					}
				}
				d.mu.Unlock()
				return fskit.DirFile(fskit.Entry("new", 0555), nodes...), nil
			}
			return &fskit.FuncFile{
				Node: fskit.Entry(name, 0555),
				ReadFunc: func(n *fskit.Node) (err error) {
					t, found := FromContext(ctx)
					if !found {
						t, err = d.Lookup("1")
						if err != nil {
							return err
						}
					}
					kind := name
					opts := TaskAllocOptions{}
					if name == linuxScratchSelector || name == linuxCapture2Selector {
						kind = "linux"
						opts.Scratch = LinuxRegularScratch
						if name == linuxCapture2Selector {
							opts.DiagnosticProfile = Capture2DiagnosticProfile
						}
					}
					scenario := memoryControlSelector(name)
					if scenario != "" {
						kind = "linux"
						opts.Scratch = LinuxRegularScratch
					}
					p, err := d.allocDerived(kind, t, opts, scenario)
					if err != nil {
						return err
					}
					fskit.SetData(n, []byte(p.ID()+"\n"))
					return nil
				},
			}, nil
		}),
	}
	fsys := vfs.New(ctx)
	if err := fsys.Bind(taskEntries{d: d, aliases: true}, ".", "."); err != nil {
		return nil, err
	}
	if err := fsys.Bind(taskEntries{d: d}, ".", "."); err != nil {
		return nil, err
	}
	if err := fsys.Bind(m, ".", "."); err != nil {
		return nil, err
	}
	t, ok := FromContext(ctx)
	if ok {
		if task, err := d.Lookup(t.ID()); err == nil {
			if err := fsys.Bind(task, ".", "self"); err != nil {
				return nil, err
			}
		}
	}
	return fsys, nil
}

func (d *TaskFS) Route(ctx context.Context, name string) (fs.FS, string, error) {
	ns, err := d.vfsNS(ctx)
	if err != nil {
		return nil, "", err
	}
	return ns.Route(ctx, name)
}

func (d *TaskFS) Stat(name string) (fs.FileInfo, error) {
	log.Println("bare stat:", name)
	return d.StatContext(context.Background(), name)
}

func (d *TaskFS) StatContext(ctx context.Context, name string) (fs.FileInfo, error) {
	ns, err := d.vfsNS(ctx)
	if err != nil {
		return nil, err
	}
	return fs.StatContext(ctx, ns, name)
}

func (d *TaskFS) Open(name string) (fs.File, error) {
	log.Println("bare open:", name)
	return d.OpenContext(context.Background(), name)
}

func (d *TaskFS) OpenContext(ctx context.Context, name string) (fs.File, error) {
	ns, err := d.vfsNS(ctx)
	if err != nil {
		return nil, err
	}
	return fs.OpenContext(ctx, ns, name)
}

// taskEntries keeps namespace bindings live while each map read uses a locked snapshot.
type taskEntries struct {
	d       *TaskFS
	aliases bool
}

func (e taskEntries) snapshot() fskit.MapFS {
	e.d.mu.Lock()
	defer e.d.mu.Unlock()
	src := e.d.resources
	if e.aliases {
		src = e.d.aliases
	}
	m := make(fskit.MapFS, len(src))
	for k, v := range src {
		m[k] = v
	}
	return m
}
func (e taskEntries) Route(ctx context.Context, name string) (fs.FS, string, error) {
	if name == "." {
		return e, name, nil
	}
	m := e.snapshot()
	target, rel, err := m.Route(ctx, name)
	if fs.Equal(target, m) {
		return e, rel, err
	}
	return target, rel, err
}
func (e taskEntries) Open(name string) (fs.File, error)     { return e.snapshot().Open(name) }
func (e taskEntries) Stat(name string) (fs.FileInfo, error) { return e.snapshot().Stat(name) }
