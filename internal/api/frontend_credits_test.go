package api

import (
	"context"
	"encoding/json"
	"testing"
)

// Server counterparts of src/utils/__tests__/credits.test.ts. Each case rolls
// back its fixture and consumption, and uses PostgreSQL's UTC clock.
func TestFrontendCreditPlans(t *testing.T) {
	f := newFrontendHarness(t)
	id, _ := f.account()
	ctx := context.Background()
	for _, tc := range []struct {
		name, setup    string
		chat, ok       bool
		source, reason string
		remaining      int
	}{
		{name: "monthly free quota", ok: true, source: "free", remaining: 0},
		{name: "welcome after free", setup: "free_used=1,welcome_credits=2", ok: true, source: "welcome", remaining: 1},
		{name: "ad after free", setup: "free_used=1,ad_credits=2", ok: true, source: "ad", remaining: 1},
		{name: "free exhausted", setup: "free_used=1", reason: "no_credits"},
		{name: "paid despite monthly cap", setup: "analyses_month=40,paid_credits=2", ok: true, source: "paid", remaining: 1},
		{name: "paid still respects daily cap", setup: "analyses_today=50,paid_credits=2", reason: "daily_cap"},
		{name: "pro remaining", setup: "plan='pro',plan_expires_at=now()+interval '1 day',analyses_month=10", ok: true, source: "pro", remaining: 29},
		{name: "pro monthly exhausted", setup: "plan='pro',plan_expires_at=now()+interval '1 day',analyses_month=40", reason: "month_cap"},
		{name: "expired pro becomes free", setup: "plan='pro',plan_expires_at=now()-interval '1 day'", ok: true, source: "free", remaining: 0},
		{name: "month rolls", setup: "period_start=(date_trunc('month',now())-interval '1 month')::date,free_used=1,analyses_month=40", ok: true, source: "free", remaining: 0},
		{name: "free has no chat", chat: true, reason: "no_plan"},
		{name: "null pro expiry has no chat", setup: "plan='pro',plan_expires_at=null", chat: true, reason: "no_plan"},
		{name: "chat-only plan", setup: "chat_expires_at=now()+interval '1 day',chat_month=40,chat_today=5", chat: true, ok: true, remaining: 109},
		{name: "pro includes chat", setup: "plan='pro',plan_expires_at=now()+interval '1 day'", chat: true, ok: true, remaining: 149},
		{name: "chat monthly cap", setup: "chat_expires_at=now()+interval '1 day',chat_month=150", chat: true, reason: "month_cap"},
		{name: "chat daily cap", setup: "chat_expires_at=now()+interval '1 day',chat_today=30", chat: true, reason: "daily_cap"},
		{name: "chat UTC daily rollover", setup: "chat_expires_at=now()+interval '1 day',chat_today=30,chat_month=40,chat_day=current_date-1", chat: true, ok: true, remaining: 109},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, e := f.s.DB.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer rollback(tx)
			_, e = tx.Exec(ctx, `update profiles set plan='free',plan_expires_at=null,chat_expires_at=null,
  period_start=date_trunc('month',now())::date,free_used=0,welcome_credits=0,paid_credits=0,ad_credits=0,
  analyses_today=0,analyses_month=0,analyses_day=current_date,chat_today=0,chat_month=0,chat_day=current_date where id=$1`, id)
			if e != nil {
				t.Fatal(e)
			}
			if tc.setup != "" {
				if _, e = tx.Exec(ctx, "update profiles set "+tc.setup+" where id=$1", id); e != nil {
					t.Fatal(e)
				}
			}
			v, e := consume(ctx, tx, id, tc.chat)
			if e != nil {
				t.Fatal(e)
			}
			if v.OK != tc.ok || v.Source != tc.source || v.Reason != tc.reason || v.Remaining != tc.remaining {
				t.Fatalf("%+v want ok=%v source=%s reason=%s remaining=%d", v, tc.ok, tc.source, tc.reason, tc.remaining)
			}
			if v.OK && tc.source == "paid" {
				var month int
				if e = tx.QueryRow(ctx, "select analyses_month from profiles where id=$1", id).Scan(&month); e != nil || month != 40 {
					t.Fatal("paid credit changed monthly use", month, e)
				}
			}
			if v.OK && tc.chat {
				var today int
				if e = tx.QueryRow(ctx, "select chat_today from profiles where id=$1", id).Scan(&today); e != nil {
					t.Fatal(e)
				}
				if tc.name == "chat UTC daily rollover" && today != 1 {
					t.Fatal(today)
				}
			}
		})
	}
}
func TestFrontendNewAccountHasThreeAnalyses(t *testing.T) {
	f := newFrontendHarness(t)
	id, _ := f.account()
	ctx := context.Background()
	for i, want := range []string{"free", "welcome", "welcome", "no_credits"} {
		tx, e := f.s.DB.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		v, e := consume(ctx, tx, id, false)
		if e != nil {
			rollback(tx)
			t.Fatal(e)
		}
		got := v.Source
		if !v.OK {
			got = v.Reason
		}
		if got != want {
			rollback(tx)
			t.Fatalf("request %d: %+v", i, v)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
	}
	// Failed operations (rollback) restore paid credits as well as counters.
	f.exec("update profiles set paid_credits=1 where id=$1", id)
	tx, e := f.s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	v, e := consume(ctx, tx, id, false)
	if e != nil || v.Source != "paid" {
		rollback(tx)
		t.Fatal(v, e)
	}
	rollback(tx)
	profile, e := rowJSON(ctx, f.s.DB, "select to_jsonb(p) from profiles p where id=$1", id)
	if e != nil {
		t.Fatal(e)
	}
	var p map[string]any
	_ = json.Unmarshal(profile, &p)
	if p["paid_credits"] != float64(1) || p["analyses_today"] != float64(3) {
		t.Fatal(p)
	}
}
