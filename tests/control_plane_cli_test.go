package workspace_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestControlPlaneCLIRejectsUnsafeConfiguration(t *testing.T) {
	binary := buildControlPlane(t)
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--listen", "0.0.0.0:8443"}, "loopback"},
		{[]string{"--listen", "localhost:8443"}, "loopback"},
		{nil, "--tls-cert and --tls-key are required"},
		{[]string{"--unknown=synthetic-cp-secret"}, "invalid Control Plane options"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		command := exec.CommandContext(ctx, binary, test.args...)
		command.Env = migrationEnvironment("")
		output, err := command.CombinedOutput()
		cancel()
		if err == nil || !strings.Contains(string(output), test.want) || strings.Contains(string(output), "synthetic-cp-secret") {
			t.Fatalf("configuration rejection: %v %s", err, output)
		}
	}
}

func TestControlPlaneCLIRequiresReadyDatabaseBeforeListening(t *testing.T) {
	binary := buildControlPlane(t)
	certFile, keyFile, _ := controlPlaneCertificate(t)
	for _, databaseURL := range []string{migrationDatabase(t), "postgres://operator:synthetic-cp-secret@127.0.0.1:1/missing?sslmode=disable"} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		command := exec.CommandContext(ctx, binary, "--listen", "127.0.0.1:0", "--tls-cert", certFile, "--tls-key", keyFile)
		command.Env = migrationEnvironment(databaseURL)
		output, err := command.CombinedOutput()
		cancel()
		if err == nil || !strings.Contains(string(output), "database") || strings.Contains(string(output), "synthetic-cp-secret") || strings.Contains(string(output), "listening") {
			t.Fatalf("database startup must fail closed: %v %s", err, output)
		}
	}
}

func TestControlPlaneCLIStartsVerifiedTLSWorkerAPI(t *testing.T) {
	_, databaseURL := workerCredentialStore(t)
	binary := buildControlPlane(t)
	certFile, keyFile, roots := controlPlaneCertificate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, binary, "--listen", "127.0.0.1:0", "--tls-cert", certFile, "--tls-key", keyFile)
	command.Env = migrationEnvironment(databaseURL)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			lines <- scanner.Text()
		} else {
			lines <- ""
		}
	}()
	var line string
	select {
	case line = <-lines:
	case <-ctx.Done():
		t.Fatal("Control Plane did not start before deadline")
	}
	const prefix = "Worker API listening on https://"
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("expected ready listener announcement, got %q", line)
	}
	origin := "https://" + strings.TrimPrefix(line, prefix)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err := client.Post(origin+"/worker/v1/claim", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal("certificate-verified Worker API:", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || response.TLS == nil || response.TLS.Version < tls.VersionTLS12 {
		t.Fatalf("expected authenticated HTTPS surface, got %d", response.StatusCode)
	}
}

func controlPlaneCertificate(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Control Plane CLI test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	directory := t.TempDir()
	certFile, keyFile := filepath.Join(directory, "server.crt"), filepath.Join(directory, "server.key")
	if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("install test CA")
	}
	return certFile, keyFile, roots
}

func buildControlPlane(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "control-plane.exe")
	command := exec.Command("go", "build", "-o", binary, "./cmd/control-plane")
	command.Dir = profileBundleRepositoryRoot(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build public Control Plane CLI: %v %s", err, output)
	}
	return binary
}
