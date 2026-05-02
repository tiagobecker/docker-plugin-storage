package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/devpower/dps/internal/driver"
	"github.com/devpower/dps/internal/store"
)

type Server struct {
	driver *driver.Driver
}

func New(d *driver.Driver) *Server {
	return &Server{driver: d}
}

func (s *Server) Listen(socket string) error {
	if err := os.MkdirAll("/run/docker/plugins", 0o755); err != nil && socket == "/run/docker/plugins/dps.sock" {
		return err
	}
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/Plugin.Activate", s.activate)
	mux.HandleFunc("/VolumeDriver.Create", s.create)
	mux.HandleFunc("/VolumeDriver.Remove", s.remove)
	mux.HandleFunc("/VolumeDriver.Mount", s.mount)
	mux.HandleFunc("/VolumeDriver.Path", s.path)
	mux.HandleFunc("/VolumeDriver.Unmount", s.unmount)
	mux.HandleFunc("/VolumeDriver.Get", s.get)
	mux.HandleFunc("/VolumeDriver.List", s.list)
	mux.HandleFunc("/VolumeDriver.Capabilities", s.capabilities)

	log.Printf("dps volume plugin listening on %s", socket)
	return http.Serve(ln, mux)
}

type errResp struct {
	Err string `json:"Err"`
}

type volumeReq struct {
	Name string            `json:"Name"`
	ID   string            `json:"ID"`
	Opts map[string]string `json:"Opts"`
}

type mountResp struct {
	Mountpoint string `json:"Mountpoint"`
	Err        string `json:"Err"`
}

type volumeInfo struct {
	Name       string            `json:"Name"`
	Mountpoint string            `json:"Mountpoint"`
	Status     map[string]string `json:"Status,omitempty"`
}

func (s *Server) activate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"Implements": []string{"VolumeDriver"}})
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req volumeReq
	if !decode(w, r, &req) {
		return
	}
	_, err := s.driver.Create(req.Name, req.Opts)
	writeErr(w, err)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	var req volumeReq
	if !decode(w, r, &req) {
		return
	}
	writeErr(w, s.driver.Remove(req.Name))
}

func (s *Server) mount(w http.ResponseWriter, r *http.Request) {
	var req volumeReq
	if !decode(w, r, &req) {
		return
	}
	v, err := s.driver.Mount(req.Name, req.ID)
	if err != nil {
		writeJSON(w, mountResp{Err: err.Error()})
		return
	}
	writeJSON(w, mountResp{Mountpoint: s.driver.VolumeDataPath(v)})
}

func (s *Server) path(w http.ResponseWriter, r *http.Request) {
	var req volumeReq
	if !decode(w, r, &req) {
		return
	}
	v, ok := s.driver.Store.GetVolume(req.Name)
	if !ok {
		writeJSON(w, mountResp{Err: os.ErrNotExist.Error()})
		return
	}
	writeJSON(w, mountResp{Mountpoint: s.driver.VolumeDataPath(v)})
}

func (s *Server) unmount(w http.ResponseWriter, r *http.Request) {
	var req volumeReq
	if !decode(w, r, &req) {
		return
	}
	writeErr(w, s.driver.Unmount(req.Name, req.ID))
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	var req volumeReq
	if !decode(w, r, &req) {
		return
	}
	v, ok := s.driver.Store.GetVolume(req.Name)
	if !ok {
		writeJSON(w, map[string]any{"Volume": nil, "Err": os.ErrNotExist.Error()})
		return
	}
	writeJSON(w, map[string]any{"Volume": s.asInfo(v), "Err": ""})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	vols := s.driver.Store.ListVolumes()
	out := make([]volumeInfo, 0, len(vols))
	for _, v := range vols {
		out = append(out, s.asInfo(v))
	}
	writeJSON(w, map[string]any{"Volumes": out, "Err": ""})
}

func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"Capabilities": map[string]string{"Scope": "local"}})
}

func (s *Server) asInfo(v *store.Volume) volumeInfo {
	status := map[string]string{
		"size":               v.Size,
		"inodes":             v.Inodes,
		"ref_count":          fmt.Sprintf("%d", v.RefCount),
		"backing_mountpoint": v.Mountpoint,
		"created_at":         v.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		"updated_at":         v.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	return volumeInfo{Name: v.Name, Mountpoint: s.driver.VolumeDataPath(v), Status: status}
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil && !errors.Is(err, os.ErrClosed) {
		writeJSON(w, errResp{Err: err.Error()})
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, err error) {
	if err != nil {
		writeJSON(w, errResp{Err: err.Error()})
		return
	}
	writeJSON(w, errResp{})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
