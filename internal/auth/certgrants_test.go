package auth

import (
	"context"
	"testing"
)

func TestCertGrants_InstanceHoldsReadImplicitlyAndRowsOnTop(t *testing.T) {
	ctx := context.Background()
	fp := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	inst := InstancePrincipal(fp)
	g := CertGrants{Store: StaticGrants{}}
	if !Allowed(ctx, g, inst, Read) {
		t.Fatal("a chained instance with no rows must hold read")
	}
	if Allowed(ctx, g, inst, Write) {
		t.Fatal("a chained instance with no rows must NOT hold write")
	}
	g = CertGrants{Store: StaticGrants{inst.String(): {Write: {}}}}
	if !Allowed(ctx, g, inst, Write) || !Allowed(ctx, g, inst, Read) {
		t.Fatal("a write row must add to the implicit read")
	}
	if !Allowed(ctx, CertGrants{}, inst, Read) {
		t.Fatal("nil Store: the implicit read still holds")
	}
}

func TestCertGrants_NoImplicitReadForAnyoneElse(t *testing.T) {
	ctx := context.Background()
	fp := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	g := CertGrants{Store: StaticGrants{}}
	for _, p := range []Principal{
		OperatorPrincipal(fp),
		{Kind: KindInstance, ID: fp, Via: ViaToken}, // an instance kind NOT vouched for by a cert
		{Kind: KindInstance, Via: ViaCert},         // no id
		{Kind: KindBridge, ID: "uid:501", Via: ViaSocket},
		{Kind: KindAnonymous, Via: ViaNone},
		{},
	} {
		if Allowed(ctx, g, p, Read) {
			t.Fatalf("%s got implicit read", p)
		}
	}
}

// Default deny on a store error holds for instances too: a database that is
// down must not leave the implicit read standing while the rows are unknown.
func TestCertGrants_StoreErrorDeniesEvenRead(t *testing.T) {
	fp := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if Allowed(context.Background(), CertGrants{Store: failingGrants{}}, InstancePrincipal(fp), Read) {
		t.Fatal("store error must deny")
	}
}
