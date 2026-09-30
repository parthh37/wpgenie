package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/store"
)

// Staff's payment notes are staff's: clients see their payments without
// them (invoice and transactions).
func TestPaymentNotesStayWithStaff(t *testing.T) {
	e := newInvoicingEnv(t)
	body := `{"amount":2000,"method":"bank","reference":"T9","note":"client disputed, see ticket 12"}`
	if c := e.as("tok", "POST", "/api/v1/invoices/"+e.id("A")+"/payments", body, nil); c != 201 {
		t.Fatalf("payment: %d", c)
	}
	var inv billing.InvoiceDetail
	e.as("tok", "GET", "/api/v1/invoices/"+e.id("A"), "", &inv)
	if len(inv.Payments) != 1 || inv.Payments[0].Note == "" {
		t.Fatalf("staff don't see the note: %+v", inv.Payments)
	}
	e.as("alice", "GET", "/api/v1/invoices/"+e.id("A"), "", &inv)
	if len(inv.Payments) != 1 || inv.Payments[0].Note != "" {
		t.Fatalf("the client sees the note: %+v", inv.Payments)
	}
	var tx []store.Payment
	e.as("alice", "GET", "/api/v1/transactions", "", &tx)
	if len(tx) != 1 || tx[0].Note != "" {
		t.Fatalf("transactions: %+v", tx)
	}
}

// The order form: billing's system names can't be taken, and a staff
// user's name is "taken" like anyone's (no different answer).
func TestOrderFormUsernames(t *testing.T) {
	e := newInvoicingEnv(t)
	if _, err := e.st.CreateUser(context.Background(), "maria", "x", "admin"); err != nil {
		t.Fatal(err)
	}
	order := func(user string) string {
		_, body := e.public("POST", "/api/v1/store/orders", fmt.Sprintf(`{"plan_id":"basic","cycle":"monthly",
			"method":"manual","contact":{"email":"jo@shop.test"},"user":{"username":%q,"password":"a long password"}}`, user),
			true, nil)
		return body
	}
	for _, name := range []string{"razorpay", "Store", "auto-pay", "automation", "billing"} {
		if body := order(name); !strings.Contains(body, `"field":"user.username"`) {
			t.Errorf("%s: %s", name, body)
		}
	}
	staff, tenant := order("maria"), order("alice")
	if staff != tenant || !strings.Contains(staff, "that username is taken") {
		t.Fatalf("staff %s, tenant %s", staff, tenant)
	}
}
