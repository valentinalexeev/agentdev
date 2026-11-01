package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyBudgets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"budgets":[{"agent":"alice","used":120,"limit":1000},{"agent":"bob","used":-1,"limit":5}]}`))
	}))
	defer srv.Close()
	list := []agent{{Name: "alice"}, {Name: "bob"}, {Name: "carol"}}
	old := budgets
	defer func() { budgets = old }()
	budgets = proxyBudgets(srv.URL, srv.Client())
	addBudgets(context.Background(), list)
	if list[0].BudgetUsed == nil || *list[0].BudgetUsed != 120 || *list[0].BudgetLimit != 1000 {
		t.Errorf("alice %+v", list[0])
	}
	if list[1].BudgetUsed != nil || list[2].BudgetUsed != nil {
		t.Error("bad or missing entry shown")
	}
	budgets = proxyBudgets("http://127.0.0.1:1/status", srv.Client()) // unreachable: no fields, no error
	list = []agent{{Name: "alice"}}
	addBudgets(context.Background(), list)
	if list[0].BudgetUsed != nil {
		t.Error("unreachable proxy gave a budget")
	}
}
