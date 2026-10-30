package daemon_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

func TestDefaultHTTPAddr(t *testing.T) {
	tests := []struct {
		name    string
		envAddr string
		want    string
	}{
		{
			name:    "MAZ_HTTP_ADDR override (real port)",
			envAddr: "127.0.0.1:9999",
			want:    "127.0.0.1:9999",
		},
		{
			name: "unset means TCP off (empty)",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAZ_HTTP_ADDR", tc.envAddr)
			if got := daemon.DefaultHTTPAddr(); got != tc.want {
				t.Errorf("DefaultHTTPAddr() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateHTTPAddr(t *testing.T) {
	tests := []struct {
		name      string
		addr      string
		want      string
		wantErr   error
		errSubstr string
	}{
		{name: "empty allowed", addr: "", want: ""},
		{name: "loopback fixed port", addr: "127.0.0.1:7842", want: "127.0.0.1:7842"},
		{name: "IPv6 fixed port", addr: "[::1]:7842", want: "[::1]:7842"},
		{name: "localhost allowed", addr: "localhost:7842", want: "localhost:7842"},
		{name: "ephemeral rejected", addr: "127.0.0.1:0", wantErr: daemon.ErrEphemeralHTTPAddr},
		{name: "all interfaces rejected", addr: "0.0.0.0:7842", wantErr: daemon.ErrNonLoopbackHTTPAddr},
		{name: "ipv6 unspecified rejected", addr: "[::]:7842", wantErr: daemon.ErrNonLoopbackHTTPAddr},
		{name: "routable IP rejected", addr: "192.168.1.10:7842", wantErr: daemon.ErrNonLoopbackHTTPAddr},
		{name: "empty host binds all interfaces rejected", addr: ":7842", wantErr: daemon.ErrNonLoopbackHTTPAddr},
		{name: "malformed rejected", addr: "garbage", errSubstr: "parse MAZ_HTTP_ADDR"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := daemon.ValidateHTTPAddr(tc.addr)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("err = %v, want one wrapping %v", err, tc.wantErr)
				}
			case tc.errSubstr != "":
				if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("err = %v, want one containing %q", err, tc.errSubstr)
				}
			default:
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				if got != tc.want {
					t.Errorf("addr = %q, want %q", got, tc.want)
				}
			}
		})
	}
}
