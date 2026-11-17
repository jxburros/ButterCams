package api

import (
	"net/http"
	"slices"
)

// Camera permissions (spec 16.6): a Viewer account can be limited to a list
// of cameras. The limit applies everywhere a camera appears; every handler
// that names a camera asks allowed, and every list is filtered with it.

// camAccess answers whether a request may see a camera.
type camAccess struct {
	limited bool
	ids     map[string]bool
}

func (a camAccess) allowed(cameraID string) bool { return !a.limited || a.ids[cameraID] }

// list returns the allowed ids for an index filter, or nil when unlimited.
func (a camAccess) list() *[]string {
	if !a.limited {
		return nil
	}
	out := make([]string, 0, len(a.ids))
	for id := range a.ids {
		out = append(out, id)
	}
	slices.Sort(out)
	return &out
}

var allCameras = camAccess{}

// accessFor returns the camera permissions of a signed-in user, read at the
// moment of the call so a change applies to the next request.
func (s *Server) accessFor(username, role string) camAccess {
	if role == RoleAdmin {
		return allCameras
	}
	ids, limited := s.users.CameraLimit(username)
	if !limited {
		return allCameras
	}
	a := camAccess{limited: true, ids: make(map[string]bool, len(ids))}
	for _, id := range ids {
		a.ids[id] = true
	}
	return a
}

// access returns the camera permissions of the request's session. A request
// without a session (the public health check from localhost) sees all.
func (s *Server) access(r *http.Request) camAccess {
	sess, ok := sessionOf(r)
	if !ok {
		return allCameras
	}
	return s.accessFor(sess.username, sess.role)
}
