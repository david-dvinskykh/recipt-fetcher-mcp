package mailbox

import (
	"strings"
	"testing"
	"time"
)

func rules(t *testing.T) Rules {
	t.Helper()
	loaded, err := LoadRules("")
	if err != nil {
		t.Fatalf("built-in rules do not load: %v", err)
	}
	return loaded
}

func TestBuiltinRulesCompile(t *testing.T) {
	loaded := rules(t)
	for _, id := range []string{"allegro", "action"} {
		if _, ok := loaded[id]; !ok {
			t.Errorf("no built-in rule for %q", id)
		}
	}
}

func TestAllegroMailParses(t *testing.T) {
	rule := rules(t)["allegro"]
	msg := Message{
		ID:      "<abc@allegro.pl>",
		From:    "no-reply@allegromail.pl",
		Subject: "Kupiłeś: Klocki konstrukcyjne",
		Date:    time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
		Text: normalizeWhitespace(`Dziękujemy za zakup!
Numer zamówienia: 1234567890
Klocki konstrukcyjne 1 szt.
Do zapłaty: 149,99 zł`),
	}

	if !rule.Matches(msg) {
		t.Fatal("the mail should match the Allegro rule")
	}
	r, ok := rule.Parse(msg)
	if !ok {
		t.Fatal("the mail should parse")
	}
	if r.Total.Decimal() != "149.99" || r.Total.Currency != "PLN" {
		t.Errorf("total = %s", r.Total)
	}
	if r.ID != "1234567890" {
		t.Errorf("id = %q, want the order number so it can match the API receipt", r.ID)
	}
	if r.Source != "email" || r.ItemsComplete {
		t.Errorf("a mail-derived receipt must say so: source=%q itemsComplete=%v", r.Source, r.ItemsComplete)
	}
	if len(r.Notes) == 0 {
		t.Error("the caller should be told this came from a mail")
	}
}

func TestAllegroRuleSkipsShippingNotices(t *testing.T) {
	rule := rules(t)["allegro"]
	msg := Message{
		From:    "no-reply@allegro.pl",
		Subject: "Twoja przesyłka została wysłana",
		Text:    "Do zapłaty: 149,99 zł",
	}
	if rule.Matches(msg) {
		t.Fatal("a shipping notice is not a purchase and would double-count the spending")
	}
}

func TestRuleIgnoresOtherSenders(t *testing.T) {
	rule := rules(t)["allegro"]
	msg := Message{From: "newsletter@example.com", Subject: "Kupiłeś", Text: "Do zapłaty: 10,00 zł"}
	if rule.Matches(msg) {
		t.Fatal("mail from an unrelated sender must not match")
	}
}

func TestActionMailParsesTotalAndItems(t *testing.T) {
	rule := rules(t)["action"]
	msg := Message{
		ID:      "<1@action.com>",
		From:    "noreply@mail.action.com",
		Subject: "Bedankt voor je aankoop - kassabon",
		Date:    time.Date(2026, 7, 15, 17, 4, 0, 0, time.UTC),
		Text: normalizeWhitespace(`Bedankt voor je aankoop
Batterijen AA 2 x €3,49
Totaal: € 12,47`),
	}

	if !rule.Matches(msg) {
		t.Fatal("the mail should match the Action rule")
	}
	r, ok := rule.Parse(msg)
	if !ok {
		t.Fatal("the mail should parse")
	}
	if r.Total.Decimal() != "12.47" || r.Total.Currency != "EUR" {
		t.Errorf("total = %s", r.Total)
	}
	if len(r.Items) != 1 {
		t.Fatalf("got %d items, want the one priced line: %+v", len(r.Items), r.Items)
	}
	if r.Items[0].Name != "Batterijen AA" || r.Items[0].Quantity != 2 || r.Items[0].Total.Decimal() != "6.98" {
		t.Errorf("item = %+v", r.Items[0])
	}
}

func TestParseFailsWithoutTotal(t *testing.T) {
	rule := rules(t)["allegro"]
	msg := Message{From: "no-reply@allegro.pl", Subject: "Kupiłeś coś", Text: "Dziękujemy za zakup!"}
	if _, ok := rule.Parse(msg); ok {
		t.Fatal("a mail with no amount is useless for categorization and must be skipped")
	}
}

func TestParseFallsBackToMessageDateAndHashedID(t *testing.T) {
	rule := rules(t)["action"]
	when := time.Date(2026, 7, 15, 17, 4, 0, 0, time.UTC)
	msg := Message{ID: "<unique@action.com>", From: "noreply@action.com", Subject: "kassabon", Date: when, Text: "Totaal: € 5,00"}

	r, ok := rule.Parse(msg)
	if !ok {
		t.Fatal("should parse")
	}
	if !r.PurchasedAt.Equal(when) {
		t.Errorf("purchased at %s, want the mail date %s", r.PurchasedAt, when)
	}
	if !strings.HasPrefix(r.ID, "mail-") {
		t.Errorf("id = %q, want a stable synthetic id", r.ID)
	}

	again, _ := rule.Parse(msg)
	if again.ID != r.ID {
		t.Error("the synthetic id must be stable across runs")
	}
}

func TestLoadRulesOverride(t *testing.T) {
	path := t.TempDir() + "/rules.json"
	const override = `[{"provider":"action","store_name":"Action","senders":["example.org"],"currency":"EUR","total_patterns":["SUM ([0-9]+[.,][0-9]{2})"]}]`
	if err := writeFile(path, override); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadRules(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded["action"].Senders; len(got) != 1 || got[0] != "example.org" {
		t.Fatalf("override did not replace the built-in rule: %v", got)
	}
	if _, ok := loaded["allegro"]; !ok {
		t.Error("an override for one store must not drop the others")
	}
}

func TestLoadRulesRejectsBadPattern(t *testing.T) {
	path := t.TempDir() + "/rules.json"
	if err := writeFile(path, `[{"provider":"action","total_patterns":["([0-9"]}]`); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRules(path); err == nil {
		t.Fatal("an invalid regexp must be reported at load time, not at the first mail")
	}
}
