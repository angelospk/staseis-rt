package server

import (
	"testing"

	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

// An OASA Stop carries its stop_code, the code on the stop sign (S3 is 10003 in the fixture).
func TestV2StopCode(t *testing.T) {
	a, _ := dailyApp(t, expiredMonday, telematics.DailySchedule{Descr: "WEST - EAST", Go: []string{"10:40"}})
	poll(a, expiredMonday)
	var s v2Stop
	v2get(t, a, "/v2/stops/"+esc("oasa:S3"), &s)
	if s.Stop.Code != "10003" {
		t.Fatalf("code %q, want 10003", s.Stop.Code)
	}
}
