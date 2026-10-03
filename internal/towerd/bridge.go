package towerd

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"

	"github.com/jerrykal/tower/internal/client"
	"github.com/jerrykal/tower/internal/config"
	"github.com/jerrykal/tower/internal/proto"
)

// Bridge is `tower towerd --stdio`, run by a home's ssh: it makes sure a
// current towerd runs here (starting it remote-only, replacing an older
// or wedged one), opens a stream call, and joins stdin and stdout to it
// until either side ends. It logs to the towerd log, never stdout.
func Bridge(e *config.Env, version string, in io.Reader, out io.Writer) error {
	logf := bridgeLog(e)
	c := client.New(e)
	c.Version = version
	c.Bridged = true
	ctx := context.Background()
	if _, err := c.Ensure(ctx); err != nil {
		logf("bridge: %v", err)
		return err
	}
	conn, err := c.Dial(ctx)
	if err != nil {
		logf("bridge: %v", err)
		return err
	}
	defer conn.Close()
	b, _ := json.Marshal(proto.Call{Op: proto.CallStream, Version: version})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return err
	}
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(conn, in)
		// The home is gone: tell towerd at once.
		if uc, ok := conn.(*net.UnixConn); ok {
			uc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(out, conn)
		done <- struct{}{}
	}()
	<-done
	return nil
}

func bridgeLog(e *config.Env) func(string, ...any) {
	f, err := os.OpenFile(e.State("towerd.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return func(string, ...any) {}
	}
	l := log.New(f, "", log.LstdFlags|log.Lmicroseconds)
	return func(format string, a ...any) { l.Printf(format, a...) }
}
