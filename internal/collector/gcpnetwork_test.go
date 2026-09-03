package collector

import (
	"strings"
	"testing"
)

const (
	netPath     = "/compute/v1/projects/p/global/networks/prod-vpc"
	subnetsPath = "/compute/v1/projects/p/regions/us-central1/subnetworks"
	prodVPC     = "projects/p/global/networks/prod-vpc"
)

func subnetJSON(name, network string) string {
	return `{"name":"` + name + `","network":"https://www.googleapis.com/compute/v1/` + network + `"}`
}

func TestResolveGcpSubnetwork(t *testing.T) {
	t.Run("an auto-mode VPC needs none", func(t *testing.T) {
		f := newGCPFake(t).on("GET", netPath, 200, `{"autoCreateSubnetworks":true}`)
		stubGCP(t, f)
		got, err := ResolveGcpSubnetwork(prodVPC, "us-central1")
		if err != nil || got != "" {
			t.Fatalf("got (%q, %v), want none", got, err)
		}
		if f.called("GET", subnetsPath) != 0 {
			t.Error("auto-mode must not list subnetworks")
		}
	})
	t.Run("the region's only subnetwork on the VPC is chosen", func(t *testing.T) {
		stubGCP(t, newGCPFake(t).
			on("GET", netPath, 200, `{"autoCreateSubnetworks":false}`).
			on("GET", subnetsPath, 200, `{"items":[`+
				subnetJSON("other", "projects/p/global/networks/other-vpc")+`,`+
				subnetJSON("prod-central", prodVPC)+`]}`))
		got, err := ResolveGcpSubnetwork(prodVPC, "us-central1")
		if err != nil || got != "projects/p/regions/us-central1/subnetworks/prod-central" {
			t.Fatalf("got (%q, %v)", got, err)
		}
	})
	t.Run("none in the region names the flag", func(t *testing.T) {
		stubGCP(t, newGCPFake(t).
			on("GET", netPath, 200, `{"autoCreateSubnetworks":false}`).
			on("GET", subnetsPath, 200, `{"items":[`+subnetJSON("other", "projects/p/global/networks/other-vpc")+`]}`))
		_, err := ResolveGcpSubnetwork(prodVPC, "us-central1")
		if err == nil || !strings.Contains(err.Error(), "--subnetwork") || !strings.Contains(err.Error(), "us-central1") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("several are listed for the operator to choose from", func(t *testing.T) {
		stubGCP(t, newGCPFake(t).
			on("GET", netPath, 200, `{"autoCreateSubnetworks":false}`).
			on("GET", subnetsPath, 200, `{"items":[`+subnetJSON("app", prodVPC)+`,`+subnetJSON("db", prodVPC)+`]}`))
		_, err := ResolveGcpSubnetwork(prodVPC, "us-central1")
		if err == nil || !strings.Contains(err.Error(), "--subnetwork") || !strings.Contains(err.Error(), "app, db") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a shared VPC is looked up in its host project", func(t *testing.T) {
		f := newGCPFake(t).
			on("GET", "/compute/v1/projects/host/global/networks/shared", 200, `{"autoCreateSubnetworks":false}`).
			on("GET", "/compute/v1/projects/host/regions/us-central1/subnetworks", 200,
				`{"items":[`+subnetJSON("shared-central", "projects/host/global/networks/shared")+`]}`)
		stubGCP(t, f)
		got, err := ResolveGcpSubnetwork("projects/host/global/networks/shared", "us-central1")
		if err != nil || got != "projects/host/regions/us-central1/subnetworks/shared-central" {
			t.Fatalf("got (%q, %v)", got, err)
		}
	})
	t.Run("a lookup failure names the flag", func(t *testing.T) {
		stubGCP(t, newGCPFake(t).on("GET", netPath, 403, gcpDeniedJSON))
		_, err := ResolveGcpSubnetwork(prodVPC, "us-central1")
		if err == nil || !strings.Contains(err.Error(), "--subnetwork") {
			t.Fatalf("err = %v", err)
		}
	})
}
