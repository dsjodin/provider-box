package server

import (
	"html/template"
	"strings"

	"github.com/dsjodin/provider-box/services/dashboard/internal/certs"
	"github.com/dsjodin/provider-box/services/dashboard/internal/dns"
	"github.com/dsjodin/provider-box/services/dashboard/internal/docker"
	"github.com/dsjodin/provider-box/services/dashboard/internal/ipam"
	"github.com/dsjodin/provider-box/services/dashboard/internal/logs"
)

// Status is a panel's health for both the template and the JSON API.
type Status struct {
	State string `json:"state"` // "ok" | "unavailable" | "disabled"
	Error string `json:"error,omitempty"`
}

func (s Status) OK() bool          { return s.State == "ok" }
func (s Status) Unavailable() bool { return s.State == "unavailable" }
func (s Status) Disabled() bool    { return s.State == "disabled" }

func ok() Status                 { return Status{State: "ok"} }
func unavailable(e error) Status { return Status{State: "unavailable", Error: e.Error()} }
func disabled(msg string) Status { return Status{State: "disabled", Error: msg} }

type Page struct {
	FQDN        string        `json:"fqdn"`
	GeneratedAt string        `json:"generated_at"`
	Certs       CertsPanel    `json:"certificates"`
	DNS         DNSPanel      `json:"dns"`
	IPAM        IPAMPanel     `json:"ipam"`
	Services    ServicesPanel `json:"services"`
	Errors      ErrorsPanel   `json:"recent_errors"`
}

type CertsPanel struct {
	Status  Status        `json:"status"`
	Summary certs.Summary `json:"summary"`
}

type DNSPanel struct {
	Status   Status       `json:"status"`
	Overview dns.Overview `json:"overview"`
}

type IPAMPanel struct {
	Status   Status        `json:"status"`
	Overview ipam.Overview `json:"overview"`
}

type ServicesPanel struct {
	Status     Status             `json:"status"`
	Containers []docker.Container `json:"containers"`
}

type ErrorsPanel struct {
	Status  Status       `json:"status"`
	Entries []logs.Entry `json:"entries"`
}

var tmplFuncs = template.FuncMap{
	"join": func(sep string, items []string) string { return strings.Join(items, sep) },
}
