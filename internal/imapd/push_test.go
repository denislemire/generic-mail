package imapd

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/denislemire/generic-mail/internal/blob"
	"github.com/denislemire/generic-mail/internal/mailbox"
	"github.com/emersion/go-imap/v2/imapserver"
)

func TestApplePushWireRegistration(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			ctx := context.Background()
			engine, err := mailbox.New(ctx, blob.NewMem(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			backend := &Server{Engine: engine, Username: "owner", Password: "password"}
			if enabled {
				backend.PushTopic = "com.apple.mobilemail.push.example"
			}
			srv := imapserver.New(&imapserver.Options{InsecureAuth: true, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return backend.NewSession(), nil, nil
			}})
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- srv.Serve(ln) }()
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { conn.Close(); srv.Close(); <-done }()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			r := bufio.NewReader(conn)
			if _, err = r.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			command := func(tag, command string) string {
				t.Helper()
				fmt.Fprintf(conn, "%s %s\r\n", tag, command)
				var out strings.Builder
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						t.Fatal(err)
					}
					out.WriteString(line)
					if strings.HasPrefix(line, tag+" ") {
						return out.String()
					}
				}
			}
			if strings.Contains(command("a", "CAPABILITY"), "XAPPLEPUSHSERVICE") {
				t.Fatal("advertised before authentication")
			}
			registration := "XAPPLEPUSHSERVICE aps-version 2 aps-account-id account aps-device-token " + strings.Repeat("ab", 32) + " aps-subtopic com.apple.mobilemail mailboxes (INBOX)"
			if strings.Contains(command("b", registration), "b OK") {
				t.Fatal("registered without authentication")
			}
			if !strings.Contains(command("c", "LOGIN owner password"), "c OK") {
				t.Fatal("login failed")
			}
			if got := strings.Contains(command("d", "CAPABILITY"), "XAPPLEPUSHSERVICE"); got != enabled {
				t.Fatalf("capability=%v", got)
			}
			out := command("e", registration)
			if strings.Contains(out, "e OK") != enabled {
				t.Fatalf("registration response %s", out)
			}
			if enabled && !strings.Contains(out, `aps-topic "com.apple.mobilemail.push.example"`) {
				t.Fatalf("missing topic: %s", out)
			}
			jobs, err := engine.PendingPush(ctx, time.Now(), 32)
			if err != nil {
				t.Fatal(err)
			}
			if (len(jobs) == 1) != enabled {
				t.Fatal("registration not persisted before acknowledgement")
			}
		})
	}
}
