package antigravity

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestAuthVerifyStagesAndSilence(t *testing.T) {
	for _, test := range []struct {
		name            string
		fixture         fakeCLIConfig
		stage, category string
	}{
		{name: "success", fixture: fakeCLIConfig{Usage: "synthetic usage", Stream: initEvent + resultEvent}},
		{name: "models", fixture: fakeCLIConfig{Models: "wrong model"}, stage: "models", category: "invalid_response"},
		{name: "usage", fixture: fakeCLIConfig{}, stage: "usage", category: "invalid_response"},
		{name: "usage process", fixture: fakeCLIConfig{ExitCode: 17}, stage: "usage", category: "command_failed"},
		{name: "generation", fixture: fakeCLIConfig{Usage: "synthetic usage", Stream: "secret-bad-protocol", Stderr: "private-cli-error"}, stage: "generation", category: "invalid_response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, _ := fakeRunner(t, test.fixture)
			err := runner.AuthVerify(context.Background())
			if test.stage == "" {
				if err != nil {
					t.Fatal(err)
				}
				c := runner.Credentials.(*runnerCredentials)
				if c.restores != 4 || c.saves != 4 {
					t.Fatalf("stage lifecycle restores=%d saves=%d", c.restores, c.saves)
				}
			} else {
				var failure *AuthError
				if !errors.As(err, &failure) || failure.Stage != test.stage || failure.Category != test.category {
					t.Fatalf("diagnostic=%v", err)
				}
				if strings.Contains(err.Error(), "private-cli-error") || strings.Contains(err.Error(), "secret-bad-protocol") {
					t.Fatal("raw CLI output leaked")
				}
				if test.name == "usage process" && failure.ExitCode != 17 {
					t.Fatalf("exit_code=%d", failure.ExitCode)
				}
			}
			assertWorkspacesClean(t, runner)
		})
	}
}

func TestAuthLoginSavesBeforeReturning(t *testing.T) {
	for _, code := range []int{0, 23} {
		runner, _ := fakeRunner(t, fakeCLIConfig{UpdatedCredential: refreshedCredential, ExitCode: code})
		stdin, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer stdin.Close()
		err = runner.AuthLogin(context.Background(), stdin, io.Discard)
		if code == 0 && err != nil {
			t.Fatal(err)
		}
		if code != 0 {
			var failure *AuthError
			if !errors.As(err, &failure) || failure.Stage != "authorization" || failure.ExitCode != code {
				t.Fatalf("diagnostic=%v", err)
			}
		}
		c := runner.Credentials.(*runnerCredentials)
		if c.data != refreshedCredential || c.saves != 1 {
			t.Fatal("authorization credential was not saved")
		}
		assertWorkspacesClean(t, runner)
	}
}

func TestAuthLoginCredentialErrorsAreSafe(t *testing.T) {
	for _, stage := range []string{"credential_restore", "credential_save"} {
		runner, _ := fakeRunner(t, fakeCLIConfig{})
		c := runner.Credentials.(*runnerCredentials)
		if stage == "credential_restore" {
			c.restoreErr = errors.New("secret-token")
		} else {
			c.saveErr = errors.New("secret-token")
		}
		err := runner.AuthLogin(context.Background(), nil, io.Discard)
		var failure *AuthError
		if !errors.As(err, &failure) || failure.Stage != stage || strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("diagnostic=%v", err)
		}
	}
}

func TestAuthLoginAllowsMissingInitialCredential(t *testing.T) {
	runner, _ := fakeRunner(t, fakeCLIConfig{UpdatedCredential: refreshedCredential})
	c := runner.Credentials.(*runnerCredentials)
	c.restoreErr = ErrCredentialsMissing
	if err := runner.AuthLogin(context.Background(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c.data != refreshedCredential || c.saves != 1 {
		t.Fatal("first authorization was not persisted")
	}
}

type cancelDuringSave struct {
	Credentials
	cancel context.CancelFunc
}

func (c cancelDuringSave) Save(ctx context.Context, home string) error {
	c.cancel()
	return c.Credentials.Save(ctx, home)
}

func TestCancellationDuringSaveDoesNotReportSuccess(t *testing.T) {
	for _, mode := range []string{"request", "login", "verify"} {
		t.Run(mode, func(t *testing.T) {
			runner, _ := fakeRunner(t, fakeCLIConfig{UpdatedCredential: refreshedCredential, Stream: initEvent + resultEvent, Usage: "usage"})
			c := runner.Credentials.(*runnerCredentials)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner.Credentials = cancelDuringSave{Credentials: c, cancel: cancel}
			if mode == "request" {
				_, failure := runner.Run(ctx, "private prompt")
				if failure == nil || failure.Status != 499 {
					t.Fatalf("canceled request returned success: %v", failure)
				}
			} else {
				var err error
				if mode == "login" {
					err = runner.AuthLogin(ctx, nil, io.Discard)
				} else {
					err = runner.AuthVerify(ctx)
				}
				var failure *AuthError
				if !errors.As(err, &failure) || failure.Category != "canceled" || failure.ExitCode != 130 {
					t.Fatalf("canceled auth returned success: %v", err)
				}
			}
			if c.data != refreshedCredential || c.saveContextErr != nil {
				t.Fatal("cancellation aborted credential cleanup")
			}
		})
	}
}
