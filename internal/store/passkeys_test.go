package store

import (
	"context"
	"errors"
	"testing"
)

// Passkeys belong to one user: a credential id is unique, another user can't change or
// remove it, and deleting the user removes their passkeys.
func TestPasskeysOwnershipUniqueAndCascade(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	ids := map[string]int64{}
	for _, name := range []string{"ann", "ben"} {
		if err := st.UpsertPendingUser(ctx, User{Username: name, PasswordHash: "x", Role: "viewer", TOTPSecret: "JBSWY3DPEHPK3PXP"}); err != nil {
			t.Fatal(err)
		}
		u, err := st.UserByName(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = u.ID
	}
	id, err := st.AddPasskey(ctx, Passkey{UserID: ids["ann"], CredentialID: "cred-1", UserHandle: "h", RPID: "m.example", Name: "Key", DataJSON: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddPasskey(ctx, Passkey{UserID: ids["ben"], CredentialID: "cred-1", UserHandle: "h2", RPID: "m.example", DataJSON: "{}"}); !errors.Is(err, ErrPasskeyTaken) {
		t.Fatalf("duplicate credential id: %v", err)
	}
	if err := st.SetPasskeyOptions(ctx, ids["ben"], id, "x", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user changed the passkey: %v", err)
	}
	if err := st.DeletePasskey(ctx, ids["ben"], id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user removed the passkey: %v", err)
	}
	if err := st.SetPasskeyOptions(ctx, ids["ann"], id, "Laptop", true); err != nil {
		t.Fatal(err)
	}
	p, err := st.PasskeyByCredentialID(ctx, "cred-1")
	if err != nil || p.Name != "Laptop" || !p.Passwordless || p.UserID != ids["ann"] {
		t.Fatalf("passkey: %+v %v", p, err)
	}
	if err := st.DeleteUser(ctx, ids["ann"]); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PasskeyByCredentialID(ctx, "cred-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("passkey survived its user: %v", err)
	}
}
