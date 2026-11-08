package main

// connect-proxy (#204): the ssh ProxyCommand of executor Jobs. ssh starts it with the target host and port; it opens
// an HTTP CONNECT tunnel through the executor egress proxy (HTTPS_PROXY, with the executor's proxy credential) and
// copies stdin/stdout. The proxy allows CONNECT only to the executor's Ready SSHTargets (executor_egress.go). Shipped
// in the operator image and copied into the Job by an init container, so the tool image needs no netcat.

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

func connectProxy(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: connect-proxy <host> <port>")
		return 2
	}
	host, port := args[0], args[1]
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		fmt.Fprintln(stderr, "connect-proxy: bad port")
		return 2
	}
	raw := os.Getenv("HTTPS_PROXY")
	if raw == "" {
		raw = os.Getenv("https_proxy")
	}
	pu, err := url.Parse(raw)
	if err != nil || pu.Host == "" || pu.Scheme != "http" {
		fmt.Fprintln(stderr, "connect-proxy: HTTPS_PROXY (http://user:key@host:port) is not set")
		return 1
	}
	conn, err := net.DialTimeout("tcp", pu.Host, 10*time.Second)
	if err != nil {
		fmt.Fprintln(stderr, "connect-proxy:", err)
		return 1
	}
	defer conn.Close()
	target := net.JoinHostPort(host, port)
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if pu.User != nil {
		pw, _ := pu.User.Password()
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(pu.User.Username()+":"+pw)) + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		fmt.Fprintln(stderr, "connect-proxy:", err)
		return 1
	}
	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		fmt.Fprintln(stderr, "connect-proxy:", err)
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "connect-proxy: the executor egress proxy refused %s (HTTP %d): is it a Ready SSHTarget of this executor?\n",
			target, resp.StatusCode)
		return 1
	}
	_ = conn.SetReadDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(conn, stdin); done <- struct{}{} }()
	go func() { _, _ = io.Copy(stdout, br); done <- struct{}{} }()
	<-done
	return 0
}

// copySelf copies the running binary to path (mode 0555), for install-helper.
func copySelf(path string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(self)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o555)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
