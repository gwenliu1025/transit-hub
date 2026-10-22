package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func testInput() repairInput {
	return repairInput{UserID: "u", AdminAccountID: "a", Rows: []revenueRow{{Date: "2026-07-12", Revenue: 100}, {Date: "2026-07-13", Revenue: 125}}}
}

func testBefore() []beforeRow {
	return []beforeRow{
		{Exists: true, ID: "one", Date: "2026-07-12", Revenue: 5, Purchase: 3, Net: 2, SiteBalance: 30, UpstreamBalance: 40, BalanceRecorded: true, CreatedAt: "2026-07-12T00:00:00Z"},
		{Exists: true, ID: "two", Date: "2026-07-13", Revenue: 6, Purchase: 4, Net: 2, SiteBalance: 31, UpstreamBalance: 41, PurchaseComplete: true, CreatedAt: "2026-07-13T00:00:00Z"},
	}
}

func TestDecodeInputStrictContract(t *testing.T) {
	now := time.Date(2026, 10, 22, 16, 0, 0, 0, time.UTC)
	valid := `{"userId":"u","adminAccountId":"a","rows":[{"date":"2026-07-12","revenue":100},{"date":"2026-07-13","revenue":125}]}`
	for name, text := range map[string]string{
		"unknown":          strings.Replace(valid, `"userId"`, `"other"`, 1),
		"case_alias":       strings.Replace(valid, `"userId"`, `"UserId"`, 1),
		"duplicate_key":    strings.Replace(valid, `"userId":"u"`, `"userId":"x","userId":"u"`, 1),
		"duplicate_amount": strings.Replace(valid, `"revenue":100`, `"revenue":1,"revenue":100`, 1),
		"duplicate_day":    strings.Replace(valid, "2026-07-13", "2026-07-12", 1),
		"gap":              strings.Replace(valid, "2026-07-13", "2026-07-14", 1),
		"missing_amount":   strings.Replace(valid, `,"revenue":100`, ``, 1),
		"negative":         strings.Replace(valid, `"revenue":100`, `"revenue":-1`, 1),
		"null":             strings.Replace(valid, `"revenue":100`, `"revenue":null`, 1),
		"nonfinite":        strings.Replace(valid, `"revenue":100`, `"revenue":1e999`, 1),
		"today":            strings.ReplaceAll(strings.ReplaceAll(valid, "2026-07-12", "2026-10-22"), "2026-07-13", "2026-10-23"),
		"missing_identity": strings.Replace(valid, `"userId":"u"`, `"userId":""`, 1),
		"trailing":         valid + `{}`,
		"oversize":         valid + strings.Repeat(" ", maxInputBytes),
		"empty_rows":       `{"userId":"u","adminAccountId":"a","rows":[]}`,
		"deep":             strings.Repeat("[", 100) + "0" + strings.Repeat("]", 100),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeInput(strings.NewReader(text), now); err == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
	got, err := decodeInput(strings.NewReader(valid), now)
	if err != nil || len(got.Rows) != 2 || got.Rows[0].Revenue != 100 {
		t.Fatalf("input=%+v error=%v", got, err)
	}
	for _, count := range []int{1, 366, 367} {
		input := repairInput{UserID: "u", AdminAccountID: "a"}
		for i := 0; i < count; i++ {
			input.Rows = append(input.Rows, revenueRow{Date: time.Date(2024, 1, 1+i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly), Revenue: 0})
		}
		raw, _ := json.Marshal(input)
		_, err := decodeInput(bytes.NewReader(raw), now)
		if (err == nil) != (count <= 366) {
			t.Fatalf("count=%d error=%v", count, err)
		}
	}
}

func TestPlanHashCoversEveryBeforeField(t *testing.T) {
	input, before := testInput(), testBefore()
	base, err := makePlan(input, before)
	if err != nil {
		t.Fatal(err)
	}
	mutators := []func(*beforeRow){
		func(r *beforeRow) { r.ID = "new" }, func(r *beforeRow) { r.Revenue++ }, func(r *beforeRow) { r.Purchase++ }, func(r *beforeRow) { r.Net++ },
		func(r *beforeRow) { r.SiteBalance++ }, func(r *beforeRow) { r.UpstreamBalance++ }, func(r *beforeRow) { r.RevenueComplete = true },
		func(r *beforeRow) { r.PurchaseComplete = true }, func(r *beforeRow) { r.BalanceRecorded = false }, func(r *beforeRow) { r.CreatedAt = "2026-07-12T00:00:01Z" },
	}
	for i, mutate := range mutators {
		rows := append([]beforeRow(nil), before...)
		mutate(&rows[0])
		p, err := makePlan(input, rows)
		if err != nil || p.PlanHash == base.PlanHash {
			t.Fatalf("field %d did not affect hash: %v", i, err)
		}
	}
	for _, missing := range [][]beforeRow{before[:1], {{Date: before[0].Date}, before[1]}} {
		if _, err := makePlan(input, missing); err == nil {
			t.Fatal("accepted missing row")
		}
	}
	input.UserID = "other"
	p, _ := makePlan(input, before)
	if p.PlanHash == base.PlanHash {
		t.Fatal("identity omitted")
	}
	input = testInput()
	input.Rows[0].Revenue++
	p, _ = makePlan(input, before)
	if p.PlanHash == base.PlanHash {
		t.Fatal("target omitted")
	}
}

// 假事务仅验证编排次数和接受状态；真实SQL合并、锁与回滚另由PostgreSQL入口验证。
type fakeTransaction struct {
	before, after                         []beforeRow
	applyCalls, reads, commits, rollbacks int
	failApply                             int
	commitErr, readErr, rollbackErr       error
}

func (tx *fakeTransaction) Read(context.Context, repairInput, bool) ([]beforeRow, error) {
	tx.reads++
	if tx.reads > 1 {
		return tx.after, tx.readErr
	}
	return tx.before, nil
}
func (tx *fakeTransaction) Apply(context.Context, repairInput, revenueRow) error {
	tx.applyCalls++
	if tx.applyCalls == tx.failApply {
		return errors.New("injected")
	}
	return nil
}
func (tx *fakeTransaction) Commit(context.Context) error   { tx.commits++; return tx.commitErr }
func (tx *fakeTransaction) Rollback(context.Context) error { tx.rollbacks++; return tx.rollbackErr }

type fakeDatabase struct {
	tx        *fakeTransaction
	apply     bool
	postReads int
	postErr   error
}

func (db *fakeDatabase) Begin(_ context.Context, apply bool) (repairTransaction, error) {
	db.apply = apply
	return db.tx, nil
}
func (db *fakeDatabase) Read(context.Context, repairInput) ([]beforeRow, error) {
	db.postReads++
	return db.tx.after, db.postErr
}
func testDatabase() *fakeDatabase {
	before := testBefore()
	after := append([]beforeRow(nil), before...)
	for i := range after {
		after[i].Revenue = testInput().Rows[i].Revenue
		after[i].RevenueComplete = true
		after[i].Net = after[i].Revenue - after[i].Purchase
		after[i].CreatedAt = "2026-10-23T00:00:00Z"
	}
	return &fakeDatabase{tx: &fakeTransaction{before: before, after: after}}
}
func TestRepairLifecycleAndFailureBoundaries(t *testing.T) {
	for _, name := range []string{"dry", "success", "drift", "missing", "partial_write", "inside_read", "changed_cost", "commit_unknown", "outside_read", "rollback_unknown"} {
		t.Run(name, func(t *testing.T) {
			db := testDatabase()
			input := testInput()
			plan, _ := makePlan(input, db.tx.before)
			expected := plan.PlanHash
			apply := name != "dry"
			wantStatus := "rolled_back"
			wantErr := true
			switch name {
			case "dry":
				expected = ""
				wantStatus = "dry_run"
				wantErr = false
			case "success":
				wantStatus = "committed_verified"
				wantErr = false
			case "drift":
				db.tx.before[0].CreatedAt = "changed"
			case "missing":
				db.tx.before = db.tx.before[:1]
			case "partial_write":
				db.tx.failApply = 2
			case "inside_read":
				db.tx.readErr = errors.New("read")
			case "changed_cost":
				db.tx.after[0].Purchase++
			case "commit_unknown":
				db.tx.commitErr = errors.New("wire lost")
				wantStatus = "commit_unknown"
			case "outside_read":
				db.postErr = errors.New("read")
				wantStatus = "committed_readback_failed"
			case "rollback_unknown":
				db.tx.failApply = 1
				db.tx.rollbackErr = errors.New("lost")
				wantStatus = "rollback_unconfirmed"
			}
			result, err := runRepair(context.Background(), db, input, apply, expected)
			if (err != nil) != wantErr || result.Status != wantStatus {
				t.Fatalf("status=%s error=%v", result.Status, err)
			}
			if db.tx.rollbacks != 1 {
				t.Fatalf("cleanup=%d", db.tx.rollbacks)
			}
			if name == "dry" && (db.apply || db.tx.applyCalls != 0 || db.tx.commits != 0 || db.postReads != 0) {
				t.Fatal("dry-run performed write/commit")
			}
			if name == "commit_unknown" && (db.tx.commits != 1 || db.postReads != 0 || db.tx.applyCalls != 2) {
				t.Fatal("unknown commit retried or misclassified")
			}
			if wantStatus == "rolled_back" && db.tx.commits != 0 {
				t.Fatal("failure committed")
			}
		})
	}
}

func TestCommandRejectsSecretsAndMissingApplyHash(t *testing.T) {
	input, _ := json.Marshal(testInput())
	for _, tc := range []struct {
		args []string
		url  string
	}{{[]string{"--input", "-", "--apply"}, ""}, {[]string{"--input", "-"}, "postgres://TOPSECRET@%"}} {
		var out bytes.Buffer
		code := command(context.Background(), tc.args, bytes.NewReader(input), &out, func(string) string { return tc.url })
		if code == 0 || strings.Contains(out.String(), "TOPSECRET") {
			t.Fatalf("code=%d output=%s", code, out.String())
		}
	}
}

func TestVerifyPreservesEveryCostAndBalanceField(t *testing.T) {
	for _, field := range []string{"purchase", "purchaseComplete", "site", "upstream", "balanceRecorded", "net", "revenue", "complete", "id"} {
		t.Run(field, func(t *testing.T) {
			db := testDatabase()
			row := &db.tx.after[0]
			switch field {
			case "purchase":
				row.Purchase++
			case "purchaseComplete":
				row.PurchaseComplete = !row.PurchaseComplete
			case "site":
				row.SiteBalance++
			case "upstream":
				row.UpstreamBalance++
			case "balanceRecorded":
				row.BalanceRecorded = !row.BalanceRecorded
			case "net":
				row.Net++
			case "revenue":
				row.Revenue++
			case "complete":
				row.RevenueComplete = false
			case "id":
				row.ID = "different"
			}
			if err := verifyRows(testInput(), db.tx.before, db.tx.after); err == nil {
				t.Fatal(fmt.Sprint("accepted changed ", field))
			}
		})
	}
}
