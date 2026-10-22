package storage

import (
	"context"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RegistryStatement is an admitted registry operation. Its representation is
// private: callers can select only these fixed SQL statements, never submit
// transaction control or another arbitrary SQL string through ConfigTx.
type RegistryStatement struct{ kind registryStatementKind }

type registryStatementKind uint8

const (
	registryStatementInvalid registryStatementKind = iota
	registrySelectSourceBinding
	registrySelectManifestEntity
	registrySelectDeviceSource
	registryInsertDevice
	registryInsertDeviceSource
	registryInsertEntity
	registryInsertSourceBinding
	registryInsertAlias
	registrySelectEntityState
	registrySelectAliasOwner
	registryClearCurrentAliases
	registrySetCurrentAlias
	registryTombstoneEntity
	registrySelectEntityAliasCount
)

func RegistrySourceBinding() RegistryStatement {
	return RegistryStatement{registrySelectSourceBinding}
}
func RegistryManifestEntity() RegistryStatement {
	return RegistryStatement{registrySelectManifestEntity}
}
func RegistryDeviceSource() RegistryStatement {
	return RegistryStatement{registrySelectDeviceSource}
}
func RegistryNewDevice() RegistryStatement {
	return RegistryStatement{registryInsertDevice}
}
func RegistryNewDeviceSource() RegistryStatement {
	return RegistryStatement{registryInsertDeviceSource}
}
func RegistryNewEntity() RegistryStatement {
	return RegistryStatement{registryInsertEntity}
}
func RegistryNewSourceBinding() RegistryStatement {
	return RegistryStatement{registryInsertSourceBinding}
}
func RegistryNewAlias() RegistryStatement {
	return RegistryStatement{registryInsertAlias}
}
func RegistryEntityState() RegistryStatement {
	return RegistryStatement{registrySelectEntityState}
}
func RegistryAliasOwner() RegistryStatement {
	return RegistryStatement{registrySelectAliasOwner}
}
func RegistryClearAliases() RegistryStatement {
	return RegistryStatement{registryClearCurrentAliases}
}
func RegistryActivateAlias() RegistryStatement {
	return RegistryStatement{registrySetCurrentAlias}
}
func RegistryRetireEntity() RegistryStatement {
	return RegistryStatement{registryTombstoneEntity}
}

// RegistryEntityAliasCount lets the configuration writer confirm that no
// historical alias was added outside its compilation input. The fixed query
// retains the borrow's string-only arguments and primitive result boundary.
func RegistryEntityAliasCount() RegistryStatement {
	return RegistryStatement{registrySelectEntityAliasCount}
}

type registryStatementSpec struct {
	sql        string
	parameters uint8
}

// These two exhaustive maps are the only source of registry mutation SQL for
// ordinary transactions and ConfigTx borrows. Arity is admitted with the SQL.
func (s RegistryStatement) querySpec() (registryStatementSpec, error) {
	switch s.kind {
	case registrySelectSourceBinding:
		return registryStatementSpec{`SELECT d.id, e.id, e.manifest_alias, e.tombstoned
			FROM public.registry_source_bindings b
			JOIN public.registry_entities e ON e.id=b.entity_id
			JOIN public.registry_devices d ON d.id=e.device_id
			WHERE b.instance_id=$1 AND b.source_device=$2 AND b.source_entity=$3`, 3}, nil
	case registrySelectManifestEntity:
		return registryStatementSpec{`SELECT id FROM public.registry_entities WHERE manifest_alias=$1`, 1}, nil
	case registrySelectDeviceSource:
		return registryStatementSpec{`SELECT device_id FROM public.registry_device_sources WHERE instance_id=$1 AND source_device=$2`, 2}, nil
	case registrySelectEntityState:
		return registryStatementSpec{`SELECT tombstoned,manifest_alias FROM public.registry_entities WHERE id=$1`, 1}, nil
	case registrySelectAliasOwner:
		return registryStatementSpec{`SELECT entity_id,current FROM public.registry_aliases WHERE alias=$1`, 1}, nil
	case registrySelectEntityAliasCount:
		return registryStatementSpec{`SELECT count(*)::text FROM public.registry_aliases WHERE entity_id=$1`, 1}, nil
	case registryStatementInvalid, registryInsertDevice, registryInsertDeviceSource, registryInsertEntity,
		registryInsertSourceBinding, registryInsertAlias, registryClearCurrentAliases,
		registrySetCurrentAlias, registryTombstoneEntity:
		return registryStatementSpec{}, &Error{kind: ConfigInvalid}
	default:
		return registryStatementSpec{}, &Error{kind: ConfigInvalid}
	}
}

func (s RegistryStatement) commandSpec() (registryStatementSpec, error) {
	switch s.kind {
	case registryInsertDevice:
		return registryStatementSpec{`INSERT INTO public.registry_devices(id) VALUES ($1)`, 1}, nil
	case registryInsertDeviceSource:
		return registryStatementSpec{`INSERT INTO public.registry_device_sources(instance_id,source_device,device_id) VALUES ($1,$2,$3)`, 3}, nil
	case registryInsertEntity:
		return registryStatementSpec{`INSERT INTO public.registry_entities(id,device_id,manifest_alias) VALUES ($1,$2,$3)`, 3}, nil
	case registryInsertSourceBinding:
		return registryStatementSpec{`INSERT INTO public.registry_source_bindings(instance_id,source_device,source_entity,entity_id) VALUES ($1,$2,$3,$4)`, 4}, nil
	case registryInsertAlias:
		return registryStatementSpec{`INSERT INTO public.registry_aliases(alias,entity_id,current) VALUES ($1,$2,true)`, 2}, nil
	case registryClearCurrentAliases:
		return registryStatementSpec{`UPDATE public.registry_aliases SET current=false WHERE entity_id=$1 AND current`, 1}, nil
	case registrySetCurrentAlias:
		return registryStatementSpec{`UPDATE public.registry_aliases SET current=true WHERE alias=$1`, 1}, nil
	case registryTombstoneEntity:
		return registryStatementSpec{`UPDATE public.registry_entities SET tombstoned=true WHERE id=$1`, 1}, nil
	case registryStatementInvalid, registrySelectSourceBinding, registrySelectManifestEntity,
		registrySelectDeviceSource, registrySelectEntityState, registrySelectAliasOwner, registrySelectEntityAliasCount:
		return registryStatementSpec{}, &Error{kind: ConfigInvalid}
	default:
		return registryStatementSpec{}, &Error{kind: ConfigInvalid}
	}
}

// QuerySQL and CommandSQL retain read-only inspection of the closed statement
// text. Execution always goes through the arity and type admission below.
func (s RegistryStatement) QuerySQL() (string, error) {
	spec, err := s.querySpec()
	return spec.sql, err
}

func (s RegistryStatement) CommandSQL() (string, error) {
	spec, err := s.commandSpec()
	return spec.sql, err
}

func nilRegistryPort(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func admittedRegistryArgs(ctx context.Context, tx pgx.Tx, spec registryStatementSpec, args []string) ([]any, error) {
	if nilRegistryPort(ctx) || nilRegistryPort(tx) || spec.sql == "" || spec.parameters < 1 || spec.parameters > 4 || len(args) != int(spec.parameters) {
		return nil, &Error{kind: ConfigInvalid}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// No caller-supplied object reaches pgx's argument dispatcher. In particular,
	// a QueryRewriter or driver option cannot arrive through this []any.
	bound := make([]any, len(args))
	for i, arg := range args {
		bound[i] = arg
	}
	return bound, nil
}

// ExecuteRegistryCommand and QueryRegistryRow accept only named, fixed SQL
// with exact-arity string parameters. The pgx transaction is supplied by the
// already-authoritative ordinary or ConfigTx owner, never by a borrow caller.
func ExecuteRegistryCommand(ctx context.Context, tx pgx.Tx, statement RegistryStatement, args ...string) (pgconn.CommandTag, error) {
	spec, err := statement.commandSpec()
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	bound, err := admittedRegistryArgs(ctx, tx, spec, args)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return tx.Exec(ctx, spec.sql, bound...)
}

func QueryRegistryRow(ctx context.Context, tx pgx.Tx, statement RegistryStatement, args ...string) (pgx.Row, error) {
	spec, err := statement.querySpec()
	if err != nil {
		return nil, err
	}
	bound, err := admittedRegistryArgs(ctx, tx, spec, args)
	if err != nil {
		return nil, err
	}
	return tx.QueryRow(ctx, spec.sql, bound...), nil
}

func registryStringDest(value any) bool {
	ptr, ok := value.(*string)
	return ok && ptr != nil
}

func registryBoolDest(value any) bool {
	ptr, ok := value.(*bool)
	return ok && ptr != nil
}

// A pgx RowScanner receives the whole Rows interface. Admit only the exact
// primitive destination shape of each known SELECT before invoking Scan.
func (s RegistryStatement) admitScan(dest []any) error {
	if len(dest) < 1 || len(dest) > 4 {
		return &Error{kind: ConfigInvalid}
	}
	valid := false
	switch s.kind {
	case registrySelectSourceBinding:
		valid = len(dest) == 4 && registryStringDest(dest[0]) && registryStringDest(dest[1]) &&
			registryStringDest(dest[2]) && registryBoolDest(dest[3])
	case registrySelectManifestEntity, registrySelectDeviceSource, registrySelectEntityAliasCount:
		valid = len(dest) == 1 && registryStringDest(dest[0])
	case registrySelectEntityState:
		valid = len(dest) == 2 && registryBoolDest(dest[0]) && registryStringDest(dest[1])
	case registrySelectAliasOwner:
		valid = len(dest) == 2 && registryStringDest(dest[0]) && registryBoolDest(dest[1])
	case registryStatementInvalid, registryInsertDevice, registryInsertDeviceSource, registryInsertEntity,
		registryInsertSourceBinding, registryInsertAlias, registryClearCurrentAliases,
		registrySetCurrentAlias, registryTombstoneEntity:
		return &Error{kind: ConfigInvalid}
	default:
		return &Error{kind: ConfigInvalid}
	}
	if !valid {
		return &Error{kind: ConfigInvalid}
	}
	return nil
}

// drainRegistryRow consumes a rejected or abandoned query using only local
// primitive destinations. No caller-supplied object is invoked by pgx.
func (s RegistryStatement) drainRegistryRow(row pgx.Row) error {
	if nilRegistryPort(row) {
		return &Error{kind: ConfigInvalid}
	}
	switch s.kind {
	case registrySelectSourceBinding:
		var device, entity, alias string
		var retired bool
		return row.Scan(&device, &entity, &alias, &retired)
	case registrySelectManifestEntity, registrySelectDeviceSource, registrySelectEntityAliasCount:
		var value string
		return row.Scan(&value)
	case registrySelectEntityState:
		var retired bool
		var alias string
		return row.Scan(&retired, &alias)
	case registrySelectAliasOwner:
		var owner string
		var current bool
		return row.Scan(&owner, &current)
	case registryStatementInvalid, registryInsertDevice, registryInsertDeviceSource, registryInsertEntity,
		registryInsertSourceBinding, registryInsertAlias, registryClearCurrentAliases,
		registrySetCurrentAlias, registryTombstoneEntity:
		return &Error{kind: ConfigInvalid}
	default:
		return &Error{kind: ConfigInvalid}
	}
}
