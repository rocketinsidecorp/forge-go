package web_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/web"
)

// level is a form value with its own text format.
type level int

func (l *level) UnmarshalText(text []byte) error {
	switch string(text) {
	case "low":
		*l = 1
	case "high":
		*l = 2
	default:
		return fmt.Errorf("unknown level %q", text)
	}
	return nil
}

type settingsForm struct {
	Name   string `form:"name"`
	Alert  bool   `form:"alert"`
	Level  level  `form:"level"`
	Backup *level `form:"backup"`
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

func TestDecodeFormBindsTypesThatParseTheirOwnText(t *testing.T) {
	t.Parallel()
	var form settingsForm
	if err := web.DecodeForm(formRequest("name=a&level=high&backup=low"), &form); err != nil {
		t.Fatal(err)
	}
	if form.Level != 2 || form.Backup == nil || *form.Backup != 1 {
		t.Fatalf("form = %+v", form)
	}
	err := web.DecodeForm(formRequest("name=a&level=extreme"), &settingsForm{})
	var problems web.FormErrors
	if !errors.As(err, &problems) || len(problems) != 1 || problems[0].Field != "level" || problems[0].Code != "invalid" {
		t.Fatalf("error = %v, want one invalid level", err)
	}
	if strings.Contains(problems[0].Message, "extreme") {
		t.Errorf("the message echoes the input: %q", problems[0].Message)
	}
	if err := web.DecodeForm(formRequest("name=a&level="), &settingsForm{}); !errors.As(err, &problems) || problems.Message("level") == "" {
		t.Errorf("a blank level: %v", err)
	}
}

func TestDecodeFormRefusesAMeaninglessCheckboxAndInvalidText(t *testing.T) {
	t.Parallel()
	var problems web.FormErrors
	// A checkbox that is present but empty is neither checked nor absent.
	if err := web.DecodeForm(formRequest("name=a&alert="), &settingsForm{}); !errors.As(err, &problems) || !problems.Has("alert") {
		t.Errorf("an empty checkbox: %v", err)
	}
	// Text that is not UTF-8 never reaches the application.
	if err := web.DecodeForm(formRequest("name=%ff%fe"), &settingsForm{}); !errors.As(err, &problems) || problems.Message("name") != "is not valid text" {
		t.Errorf("invalid UTF-8: %v", err)
	}
	if problems.Message("no_such_field") != "" || problems.Has("no_such_field") {
		t.Error("an unknown field has a message")
	}
}

func TestParseFormReportsABodyThatCannotBeRead(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "/settings", brokenBody{})
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_, err := web.ParseForm(request)
	public := fault.From(err)
	if public == nil || public.Code != "unreadable_body" || public.Status() != http.StatusBadRequest {
		t.Fatalf("ParseForm = %v", err)
	}
	if strings.Contains(public.Message, "reset by peer") {
		t.Errorf("the transport error reached the client: %q", public.Message)
	}
}
