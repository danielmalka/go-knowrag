package embed

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestProfile_HasNoModelRevisionField is the structural half of S04 T7: the pinned revision cannot
// diverge per deployment because there is nowhere in configuration for a revision to live. A
// runtime check would be weaker — this fails the moment someone adds the field back.
func TestProfile_HasNoModelRevisionField(t *testing.T) {
	typ := reflect.TypeOf(Profile{})
	for i := range typ.NumField() {
		name := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(name, "revision") || strings.Contains(name, "model") {
			t.Errorf("Profile has field %q — the BGE-M3 revision is pinned in code "+
				"(schema.BGEM3Revision, PRD-contrato §2.3b) and must not be configurable",
				typ.Field(i).Name)
		}
	}
}

func TestParseProfile_RejectsModelRevisionKey(t *testing.T) {
	for _, key := range []string{"model_revision", "modelrevision", "revision"} {
		t.Run(key, func(t *testing.T) {
			yaml := "endpoint: http://127.0.0.1:8080\n" + key + ": deadbeef\n"
			_, err := ParseProfile([]byte(yaml))
			if err == nil {
				t.Fatalf("config declaring %q was accepted", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q does not name the offending key %q", err, key)
			}
		})
	}
}

func TestParseProfile_AcceptsKnownKeys(t *testing.T) {
	yaml := `endpoint: http://127.0.0.1:8080
timeout: 30s
batch_size: 32
max_concurrent: 2
max_retries: 3
`
	got, err := ParseProfile([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseProfile: %v", err)
	}
	want := Profile{
		Endpoint:      "http://127.0.0.1:8080",
		Timeout:       30 * time.Second,
		BatchSize:     32,
		MaxConcurrent: 2,
		MaxRetries:    3,
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestProfile_Validate(t *testing.T) {
	valid := Profile{
		Endpoint:      "http://127.0.0.1:8080",
		Timeout:       30 * time.Second,
		BatchSize:     32,
		MaxConcurrent: 2,
		MaxRetries:    3,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid profile was rejected: %v", err)
	}

	cases := map[string]func(*Profile){
		"Endpoint":      func(p *Profile) { p.Endpoint = "" },
		"Timeout":       func(p *Profile) { p.Timeout = 0 },
		"BatchSize":     func(p *Profile) { p.BatchSize = 0 },
		"MaxConcurrent": func(p *Profile) { p.MaxConcurrent = 0 },
		"MaxRetries":    func(p *Profile) { p.MaxRetries = 0 },
	}
	for field, break_ := range cases {
		t.Run(field, func(t *testing.T) {
			p := valid
			break_(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("profile with a broken %s was accepted", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("error %q does not name %s", err, field)
			}
		})
	}
}
