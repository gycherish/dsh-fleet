package uplink

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/gycherish/dsh-fleet/pkg/envelope"
)

type fakeEnroller struct {
	owner    uuid.UUID
	authErr  error
	claimErr error
	sawUser  string
	sawNode  string
	sawOwner uuid.UUID
	claims   int
}

func (f *fakeEnroller) AuthenticateToken(_ context.Context, username, _ string) (uuid.UUID, error) {
	f.sawUser = username
	return f.owner, f.authErr
}

func (f *fakeEnroller) Claim(_ context.Context, nodeID string, ownerID uuid.UUID, _ string) error {
	f.claims++
	f.sawNode, f.sawOwner = nodeID, ownerID
	return f.claimErr
}

func hello(username string) *envelope.Hello {
	return &envelope.Hello{
		T: envelope.THello, Protocol: envelope.ProtocolVersion,
		NodeID: "selfmade", Username: username, Token: "ut_whatever",
	}
}

// A hello that names a user must go through enrolment, not machine-token auth.
func TestUserTokenGoesToEnrolment(t *testing.T) {
	owner := uuid.New()
	fake := &fakeEnroller{owner: owner}
	h := &Handler{Enrol: fake}

	if err := h.enrol(context.Background(), hello("admin")); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if fake.sawUser != "admin" || fake.sawNode != "selfmade" || fake.sawOwner != owner {
		t.Fatalf("enrolled with (%q, %q, %v), want (admin, selfmade, %v)",
			fake.sawUser, fake.sawNode, fake.sawOwner, owner)
	}
}

// A bad token must never reach the claim: otherwise presenting any token at all
// would register a machine name.
func TestABadTokenClaimsNothing(t *testing.T) {
	refused := errors.New("bad token")
	fake := &fakeEnroller{authErr: refused}
	h := &Handler{Enrol: fake}

	if err := h.enrol(context.Background(), hello("admin")); !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the refusal", err)
	}
	if fake.claims != 0 {
		t.Fatalf("claimed %d times after a refused token, want 0", fake.claims)
	}
}

// A name held by somebody else is reported, not taken over.
func TestAForeignNameIsReported(t *testing.T) {
	taken := errors.New("nodes: that machine id belongs to another account")
	fake := &fakeEnroller{owner: uuid.New(), claimErr: taken}
	h := &Handler{Enrol: fake, Foreign: taken}

	if err := h.enrol(context.Background(), hello("someone-else")); !errors.Is(err, taken) {
		t.Fatalf("err = %v, want the foreign-name error", err)
	}
}

// A deployment that wants machine tokens only leaves Enrol nil, and a user
// token must then be refused rather than panicking on it.
func TestUserTokensRefusedWhenEnrolmentIsOff(t *testing.T) {
	h := &Handler{}
	if err := h.enrol(context.Background(), hello("admin")); err == nil {
		t.Fatal("a control plane with no enroller must refuse a user token")
	}
}
