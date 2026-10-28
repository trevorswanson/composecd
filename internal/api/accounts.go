package api

import (
	"net/http"

	"github.com/trevorswanson/composecd/internal/auth"
)

// accountRoutes serves API tokens and local users. Managing them needs admin;
// a local user may change their own password.
func (s *server) accountRoutes(read, act func(string, handler)) {
	act("GET /api/v1/tokens", func(*http.Request, *auth.Principal) (int, interface{}, error) {
		ts, err := s.Auth.Store.Tokens()
		return http.StatusOK, ts, err
	})
	act("POST /api/v1/tokens", func(r *http.Request, p *auth.Principal) (int, interface{}, error) {
		var body struct {
			Name string `json:"name"`
			Role string `json:"role"`
		}
		if err := decode(r, &body); err != nil {
			return 0, nil, err
		}
		role, err := auth.ParseRole(body.Role)
		if err != nil {
			return 0, nil, badRequest("%v", err)
		}
		tok, err := s.Auth.CreateToken(body.Name, role, p.Name)
		if err != nil {
			return 0, nil, err
		}
		return http.StatusCreated, map[string]string{"name": body.Name, "role": string(role), "token": tok}, nil
	})
	act("DELETE /api/v1/tokens/{name}", func(r *http.Request, _ *auth.Principal) (int, interface{}, error) {
		err := s.Auth.RevokeToken(r.PathValue("name"))
		return http.StatusOK, map[string]string{"status": "revoked"}, err
	})

	act("GET /api/v1/users", func(*http.Request, *auth.Principal) (int, interface{}, error) {
		us, err := s.Auth.Store.Users()
		return http.StatusOK, us, err
	})
	act("POST /api/v1/users", func(r *http.Request, _ *auth.Principal) (int, interface{}, error) {
		var body struct {
			Name     string `json:"name"`
			Role     string `json:"role"`
			Password string `json:"password"`
		}
		if err := decode(r, &body); err != nil {
			return 0, nil, err
		}
		role, err := auth.ParseRole(body.Role)
		if err != nil {
			return 0, nil, badRequest("%v", err)
		}
		if err := s.Auth.AddUser(body.Name, role, body.Password); err != nil {
			return 0, nil, err
		}
		return http.StatusCreated, map[string]string{"name": body.Name, "role": string(role)}, nil
	})
	read("PUT /api/v1/users/{name}/password", func(r *http.Request, p *auth.Principal) (int, interface{}, error) {
		name := r.PathValue("name")
		self := p.Via == auth.ViaPassword && p.Name == name
		if !self && !p.Role.Allows(auth.RoleAdmin) {
			return 0, nil, &apiError{status: http.StatusForbidden, msg: "you may only change your own password"}
		}
		var body struct {
			Password string `json:"password"`
		}
		if err := decode(r, &body); err != nil {
			return 0, nil, err
		}
		err := s.Auth.SetPassword(name, body.Password)
		return http.StatusOK, map[string]string{"status": "password changed; sessions signed out"}, err
	})
	act("DELETE /api/v1/users/{name}", func(r *http.Request, _ *auth.Principal) (int, interface{}, error) {
		err := s.Auth.Store.DeleteUser(r.PathValue("name"))
		return http.StatusOK, map[string]string{"status": "deleted"}, err
	})
}
