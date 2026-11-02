package tcp

import (
	"context"
	"log"
	"strconv"
	"sync"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
)

type Service struct {
	mu        sync.RWMutex
	resources map[string]fs.FS
	nextID    int

	AllocHook func(s *Service, rid string) error
}

func New() *Service {
	return &Service{
		resources: make(map[string]fs.FS),
		nextID:    0,
	}
}

func (s *Service) Open(name string) (fs.File, error) {
	return s.OpenContext(context.Background(), name)
}

func (s *Service) OpenContext(ctx context.Context, name string) (fs.File, error) {
	return fs.OpenContext(ctx, s.rootFS(), name)
}

func (s *Service) Stat(name string) (fs.FileInfo, error) {
	return s.StatContext(context.Background(), name)
}

func (s *Service) StatContext(ctx context.Context, name string) (fs.FileInfo, error) {
	return fs.StatContext(ctx, s.rootFS(), name)
}

func (s *Service) rootFS() fskit.UnionFS {
	root := fskit.MapFS{
		"new": fskit.OpenFunc(func(ctx context.Context, name string) (fs.File, error) {
			if name != "." {
				return nil, fs.ErrNotExist
			}
			return &fskit.FuncFile{
				Node: fskit.Entry("new", 0555),
				ReadFunc: func(n *fskit.Node) error {
					rid, err := s.Alloc()
					if err != nil {
						log.Printf("KDBG2 new: alloc error %v", err) // KDBG2 (temporary)
						return err
					}
					if s.AllocHook != nil {
						if err := s.AllocHook(s, rid); err != nil {
							log.Printf("KDBG2 new: allochook error rid=%s %v", rid, err) // KDBG2 (temporary)
							return err
						}
					}
					fskit.SetData(n, []byte(rid+"\n"))
					log.Printf("KDBG2 new: allocated rid=%s", rid) // KDBG2 (temporary)
					return nil
				},
			}, nil
		}),
	}
	return fskit.UnionFS{root, fskit.MapFS(s.resources)}
}

func (s *Service) Route(_ context.Context, name string) (fs.FS, string, error) {
	if !fs.ValidPath(name) {
		return nil, "", &fs.PathError{Op: "route", Path: name, Err: fs.ErrInvalid}
	}
	return s, name, nil
}

func (s *Service) Get(rid string) (*Conn, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res, ok := s.resources[rid]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return res.(*Conn), nil
}

func (s *Service) Alloc() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	rid := strconv.Itoa(s.nextID)
	s.resources[rid] = newConn(rid, s)
	return rid, nil
}

func (s *Service) remove(rid string) {
	s.mu.Lock()
	res, ok := s.resources[rid]
	if ok {
		delete(s.resources, rid)
	}
	s.mu.Unlock()

	if ok {
		res.(*Conn).shutdown()
	}
}

// CloseAll shuts down every connection when its owning task loses the network grant.
func (s *Service) CloseAll() {
	s.mu.RLock()
	ids := make([]string, 0, len(s.resources))
	for id := range s.resources {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	log.Printf("KDBG2 CloseAll: closing %d conns", len(ids)) // KDBG2 (temporary)
	for _, id := range ids {
		s.remove(id)
	}
}
