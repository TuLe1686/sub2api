package handler

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

func TestOpsCaptureWriterForwardsBodyReadDeadlines(t *testing.T) {
	for _, useHTTP2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP1", true: "HTTP2"}[useHTTP2], func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.POST("/input", func(c *gin.Context) {
				original := c.Writer
				capture := acquireOpsCaptureWriter(original)
				c.Writer = capture
				defer func() { c.Writer = original; releaseOpsCaptureWriter(capture) }()
				c.Next()
			}, middleware.RequestMemoryGuard(config.GatewayRequestMemoryGuardConfig{
				Enabled: true, BudgetBytes: 1 << 20, ReadTimeoutSeconds: 1,
			}, 1<<20), func(c *gin.Context) { t.Error("slow input reached handler") })
			srv := httptest.NewUnstartedServer(r)
			srv.EnableHTTP2 = useHTTP2
			if useHTTP2 {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()
			if !useHTTP2 {
				conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
				_, _ = io.WriteString(conn, "POST /input HTTP/1.1\r\nHost: test\r\nContent-Length: 100\r\n\r\nx")
				buf := make([]byte, 2048)
				n, err := conn.Read(buf)
				if err != nil || !strings.Contains(string(buf[:n]), "408") {
					t.Fatalf("wrapped read deadline: %q %v", buf[:n], err)
				}
				return
			}
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			go func() { _, _ = io.WriteString(writer, "x") }()
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/input", reader)
			req.ContentLength = 100
			client := srv.Client()
			client.Timeout = 4 * time.Second
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusRequestTimeout || resp.ProtoMajor != 2 {
				t.Fatalf("wrapped HTTP2 deadline: %d %s", resp.StatusCode, resp.Proto)
			}
		})
	}
}
