// Package cli implements the separately invoked Homeward operator client.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	"github.com/alecthomas/kong"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	integration "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Exit codes are stable: 0 success/help, 1 local/I/O, 2 usage, 3 authentication,
// 4 permission, 5 conflict, 6 unavailable, 7 unsupported login outcome,
// 8 mutation not committed, 9 commit unknown, 10 reconciliation/restart required.
const (
	ExitOK              = 0
	ExitLocal           = 1
	ExitUsage           = 2
	ExitAuthentication  = 3
	ExitPermission      = 4
	ExitConflict        = 5
	ExitUnavailable     = 6
	ExitChallenge       = 7
	ExitMutationFailed  = 8
	ExitMutationUnknown = 9
	ExitMutationPending = 10
)

type failure struct {
	code string
	exit int
}

func (e *failure) Error() string       { return e.code }
func fail(code string, exit int) error { return &failure{code: code, exit: exit} }

type grammar struct {
	ConfigDir string `help:"Private profile directory (default: OS user config directory/homeward)."`
	Profile   string `help:"Explicit profile name; required for every server command."`
	Profiles  struct {
		Add  profileAdd  `cmd:"" help:"Add an immutable HTTPS or protected Unix socket profile."`
		List profileList `cmd:"" help:"List profile names and endpoints, without credentials."`
	} `cmd:"" help:"Manage local profiles; no default server is selected."`
	Login    loginCommand  `cmd:"" help:"Read password without echo and save an interactive session."`
	Logout   logoutCommand `cmd:"" help:"Revoke the remote session, then remove local credentials."`
	Sessions struct {
		List   sessionsList   `cmd:"" help:"List sessions for an account."`
		Revoke sessionsRevoke `cmd:"" help:"Revoke one session."`
	} `cmd:""`
	Accounts struct {
		List   accountsList   `cmd:""`
		Get    accountsGet    `cmd:""`
		Create accountsCreate `cmd:"" help:"Create an account, reading its password without echo."`
		Invite accountsInvite `cmd:"" help:"Create an invitation; save its one-time token in a private file."`
		Reset  recoverCommand `cmd:"" help:"Reset a password only through the protected local operator socket."`
	} `cmd:""`
	Bootstrap    bootstrapCommand `cmd:"" help:"Create the first admin through the protected local operator socket."`
	Recovery     recoverCommand   `cmd:"" help:"Recover an account password through the protected local operator socket."`
	Integrations struct {
		Catalogue integrationCatalogue `cmd:""`
		Schema    integrationSchema    `cmd:""`
		List      integrationList      `cmd:""`
		Get       integrationGet       `cmd:""`
		Health    integrationHealth    `cmd:"" help:"Read the instance report and supervision metadata."`
		Create    integrationCreate    `cmd:""`
		Update    integrationUpdate    `cmd:""`
		Delete    integrationDelete    `cmd:""`
		Enable    integrationEnable    `cmd:""`
		Disable   integrationDisable   `cmd:""`
	} `cmd:"" help:"Administer integrations through the common Connect service."`
}

type invocation struct {
	ctx      context.Context
	args     *grammar
	out      io.Writer
	terminal Terminal
	dir      *privateDirectory
}

type helpExit int

// Run never prints raw argv, passwords, bearer tokens, or server error messages.
// Results and errors are JSON; Kong alone renders contextual --help text.
func Run(ctx context.Context, argv []string, out, diagnostics io.Writer, terminal Terminal) (code int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if status, ok := recovered.(helpExit); ok {
				code = int(status)
				return
			}
			panic(recovered)
		}
	}()
	if ctx == nil || out == nil || diagnostics == nil || terminal == nil {
		return ExitLocal
	}
	args := &grammar{}
	parser, err := kong.New(args, kong.Name("homeward"), kong.Description("Homeward remote administration. JSON results; exit codes 0/1/2/3/4/5/6/7/8/9/10: success/local/usage/authentication/permission/conflict/unavailable/unsupported-factor/not-committed/commit-unknown/reconciliation-or-restart-required. Passwords are read only from a terminal."), kong.Writers(out, diagnostics), kong.Exit(func(status int) { panic(helpExit(status)) }))
	if err != nil {
		return writeFailure(diagnostics, fail("parser_unavailable", ExitLocal))
	}
	parsed, err := parser.Parse(argv)
	if err != nil {
		// Kong parse errors can contain arbitrary argument values. Do not echo them.
		return writeFailure(diagnostics, fail("invalid_arguments_use_help", ExitUsage))
	}
	v := &invocation{ctx: ctx, args: args, out: out, terminal: terminal}
	defer func() {
		if v.dir != nil {
			_ = v.dir.close()
		}
	}()
	if err := parsed.Run(v); err != nil {
		return writeFailure(diagnostics, err)
	}
	return ExitOK
}

func writeFailure(out io.Writer, err error) int {
	code, exit := "local_operation_failed", ExitLocal
	var local *failure
	if errors.As(err, &local) {
		code, exit = local.code, local.exit
	} else {
		var remote *connect.Error
		if errors.As(err, &remote) {
			code = "rpc_" + remote.Code().String()
			switch remote.Code() {
			case connect.CodeUnauthenticated:
				exit = ExitAuthentication
			case connect.CodePermissionDenied:
				exit = ExitPermission
			case connect.CodeAlreadyExists, connect.CodeAborted, connect.CodeFailedPrecondition:
				exit = ExitConflict
			case connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodeOutOfRange:
				exit = ExitUsage
			case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeCanceled, connect.CodeResourceExhausted:
				exit = ExitUnavailable
			default:
				exit = ExitLocal
			}
			// Only known typed details are emitted, never remote.Message().
			for _, detail := range remote.Details() {
				value, decodeErr := detail.Value()
				if decodeErr != nil {
					continue
				}
				switch typed := value.(type) {
				case *auth.AuthError:
					if name, ok := auth.AuthErrorCode_name[int32(typed.Code)]; ok && typed.Code != 0 {
						code += ":" + name
					}
				case *integration.IntegrationError:
					if name, ok := integration.ErrorCode_name[int32(typed.Code)]; ok && typed.Code != 0 {
						code += ":" + name
					}
				}
			}
		}
	}
	_ = json.NewEncoder(out).Encode(struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}{Error: struct {
		Code string `json:"code"`
	}{Code: code}})
	return exit
}

func (v *invocation) directory() (*privateDirectory, error) {
	if v.dir != nil {
		return v.dir, nil
	}
	path := v.args.ConfigDir
	if path == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, fail("config_directory_unavailable", ExitLocal)
		}
		path = filepath.Join(base, "homeward")
	}
	dir, err := openPrivateDirectory(path, true)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(dir.fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = dir.close()
		return nil, fail("profile_directory_busy", ExitConflict)
	}
	v.dir = dir
	return dir, nil
}

func (v *invocation) emit(value proto.Message) error {
	encoded, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(value)
	if err != nil {
		return fail("invalid_server_response", ExitLocal)
	}
	_, err = fmt.Fprintf(v.out, "{\"result\":%s}\n", encoded)
	return err
}

func (v *invocation) emitJSON(value any) error {
	return json.NewEncoder(v.out).Encode(struct {
		Result any `json:"result"`
	}{Result: value})
}

type jsonMessage struct{ proto.Message }

func (m jsonMessage) MarshalJSON() ([]byte, error) {
	return protojson.MarshalOptions{UseProtoNames: true}.Marshal(m.Message)
}
