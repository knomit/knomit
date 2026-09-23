package auth

import (
	"context"
	"net"
	"testing"
)

func TestTLSConnContext_MarksEveryConn_PlainConnContextMarksNone(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if !IsTLSListener(TLSConnContext(context.Background(), a)) {
		t.Fatal("TLSConnContext did not mark the connection")
	}
	if IsTLSListener(ConnContext(context.Background(), a)) {
		t.Fatal("the plaintext server's ConnContext marked a connection as TLS")
	}
	if IsTLSListener(context.Background()) {
		t.Fatal("an unmarked context reads as TLS")
	}
}

func TestListenTLS_RefusesNilConfig(t *testing.T) {
	if _, _, err := ListenTLS("127.0.0.1:0", nil); err == nil {
		t.Fatal("ListenTLS opened a listener with no tls.Config")
	}
}

func TestCertPrincipals_UseTheFullFingerprintAndViaCert(t *testing.T) {
	fp := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got := InstancePrincipal(fp).String(); got != "instance:"+fp+"@cert" {
		t.Fatalf("InstancePrincipal = %s", got)
	}
	if got := OperatorPrincipal(fp).String(); got != "operator:"+fp+"@cert" {
		t.Fatalf("OperatorPrincipal = %s", got)
	}
}
