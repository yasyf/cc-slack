package main

import "testing"

func TestAnAvatarInAnOlderStyleIsRebuiltOnTheNextProfileRead(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db().ExecContext(f.ctx, `UPDATE identity SET avatar_url = 'https://public.example.com/cc-slack/avatars/T/U/0123-parens.png'`); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(profileTTL)
	id, err := loadIdentity(f.ctx, f.db())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.post(f.ctx, f.db(), id, testChannel, "", "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.slack.sent()[0].iconURL; got != "https://avatars/ym.png?agent-badge" {
		t.Fatalf("icon = %q, want the old avatar rebuilt with the badge", got)
	}
}
