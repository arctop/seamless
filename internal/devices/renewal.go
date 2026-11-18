package devices

// A relay pins the exact certificate the server presented when it paired
// (owner decision R-D2, 2026-11-15: no SPKI pin, since `tailscale cert` and
// certbot without --reuse-key issue a new key at every renewal). So a renewal
// means pairing every relay again, and the server says so ahead of time.

import (
	"time"

	"github.com/arctop/seamless/internal/store"
)

// RelayRenewalWarning is how long before the served certificate's notAfter
// the server's doctor and its Devices page say that every paired relay must
// pair again after the renewal. It sits inside doctor's tls row's own warning
// (30 days, tlsExpiryWarning in cmd/seamlessd), which says the certificate is
// due; this one names the relays a renewal will refuse.
const RelayRenewalWarning = 14 * 24 * time.Hour

// Renewal is a coming renewal of the served certificate, and the relays it
// will refuse until each pairs again.
type Renewal struct {
	// NotAfter is the served certificate's.
	NotAfter time.Time `json:"not_after"`
	// Days is how many whole days are left, 0 on the last; Expired says the
	// certificate is past its notAfter.
	Days    int  `json:"days"`
	Expired bool `json:"expired"`
	// Relays are the active relays' names, as the Devices page lists them.
	Relays []string `json:"relays"`
}

// RenewalDue is the renewal warning at now for a served certificate valid
// until notAfter and the devices in list: nil before the window
// (RelayRenewalWarning), without a certificate (notAfter zero), and while no
// relay is paired, since a key device pins nothing.
func RenewalDue(notAfter time.Time, list []store.Device, now time.Time) *Renewal {
	if notAfter.IsZero() || notAfter.Sub(now) > RelayRenewalWarning {
		return nil
	}
	var relays []string
	for _, d := range list {
		if d.Kind == KindRelay && d.StatusAt(now) == store.DeviceActive {
			relays = append(relays, d.Name)
		}
	}
	if len(relays) == 0 {
		return nil
	}
	left := notAfter.Sub(now)
	r := &Renewal{NotAfter: notAfter.UTC(), Relays: relays, Expired: left <= 0}
	if left > 0 {
		r.Days = int(left / (24 * time.Hour))
	}
	return r
}
