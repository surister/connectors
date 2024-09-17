package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	boilerplate "github.com/estuary/connectors/materialize-boilerplate"
	pf "github.com/estuary/flow/go/protocols/flow"
	log "github.com/sirupsen/logrus"
)

type catalog struct {
	cfg *config
	// TODO(whb): Including the lastSpec from the validate or apply request is a temporary hack
	// until we get around to removing the "load/persist a spec in the destination" concept more
	// thoroughly. As of this writing, the iceberg materialization is the first one to actually use
	// the lastSpec from the validate or apply request.
	lastSpec      *pf.MaterializationSpec
	resourcePaths [][]string
}

func newCatalog(cfg config, resourcePaths [][]string, lastSpec *pf.MaterializationSpec) *catalog {
	return &catalog{
		cfg:           &cfg,
		resourcePaths: resourcePaths,
		lastSpec:      lastSpec,
	}
}

func (c *catalog) infoSchema() (*boilerplate.InfoSchema, error) {
	is := boilerplate.NewInfoSchema(
		func(rp []string) []string { return rp },
		func(f string) string { return f },
	)

	if len(c.resourcePaths) == 0 {
		// No bindings so there are no tables that we care about; nothing to do.
		return is, nil
	}

	pathsJson, err := json.Marshal(c.resourcePaths)
	if err != nil {
		return nil, fmt.Errorf("marshaling paths: %w", err)
	}

	b, err := runIcebergctl(c.cfg, "info-schema", string(pathsJson))
	if err != nil {
		return nil, err
	}

	var got map[string][]existingIcebergColumn
	if err := json.Unmarshal(b, &got); err != nil {
		return nil, err
	}

	for res, fields := range got {
		parts := strings.Split(res, ".")
		namespace := parts[0]
		table := parts[1]
		is.PushResource(namespace, table)

		for _, f := range fields {
			is.PushField(boilerplate.EndpointField{
				Name:     f.Name,
				Nullable: f.Nullable,
				Type:     string(f.Type),
			}, namespace, table)
		}
	}

	return is, nil
}

func (c *catalog) tablePath(resourcePath []string) (string, error) {
	b, err := runIcebergctl(c.cfg, "table-path", pathToFQN(resourcePath))
	if err != nil {
		return "", err
	}

	return string(b), nil
}

func (c *catalog) listNamespaces() ([]string, error) {
	var got []string

	if b, err := runIcebergctl(c.cfg, "list-namespaces"); err != nil {
		return nil, err
	} else if err := json.Unmarshal(b, &got); err != nil {
		return nil, err
	}

	return got, nil
}

func (c *catalog) createNamespace(namespace string) error {
	_, err := runIcebergctl(c.cfg, "create-namespace", namespace)
	return err
}

func (c *catalog) CreateResource(_ context.Context, spec *pf.MaterializationSpec, bindingIndex int) (string, boilerplate.ActionApplyFn, error) {
	b := spec.Bindings[bindingIndex]

	tc := tableCreate{Location: tablePath(c.cfg.Bucket, c.cfg.Prefix, b.ResourcePath[0], b.ResourcePath[1])}

	parquetSchema, err := parquetSchema(b.FieldSelection.AllFields(), b.Collection, b.FieldSelection.FieldConfigJsonMap)
	if err != nil {
		return "", nil, err
	}

	for _, f := range parquetSchema {
		tc.Fields = append(tc.Fields, existingIcebergColumn{
			Name:     f.Name,
			Nullable: !f.Required,
			Type:     parquetTypeToIcebergType(f.DataType),
		})
	}

	input, err := json.Marshal(tc)
	if err != nil {
		return "", nil, err
	}

	fqn := pathToFQN(b.ResourcePath)

	return fmt.Sprintf("create table %q", fqn), func(_ context.Context) error {
		if _, err := runIcebergctl(c.cfg, "create-table", fqn, string(input)); err != nil {
			return fmt.Errorf("creating table %q: %w", fqn, err)
		}

		return nil
	}, nil
}

func (c *catalog) DeleteResource(_ context.Context, path []string) (string, boilerplate.ActionApplyFn, error) {
	fqn := pathToFQN(path)

	return fmt.Sprintf("drop table %q", fqn), func(_ context.Context) error {
		if _, err := runIcebergctl(c.cfg, "drop-table", fqn); err != nil {
			return fmt.Errorf("dropping table %q: %w", fqn, err)
		}

		return nil
	}, nil
}

func (c *catalog) UpdateResource(_ context.Context, spec *pf.MaterializationSpec, bindingIndex int, bindingUpdate boilerplate.BindingUpdate) (string, boilerplate.ActionApplyFn, error) {
	if len(bindingUpdate.NewProjections) == 0 && len(bindingUpdate.NewlyNullableFields) == 0 {
		// Nothing to do, since only adding new columns or dropping nullability
		// constraints is supported currently.
		return "", nil, nil
	}

	b := spec.Bindings[bindingIndex]

	ta := tableAlter{}

	for _, f := range bindingUpdate.NewlyNullableFields {
		ta.NewlyNullableColumns = append(ta.NewlyNullableColumns, f.Name)
	}

	for _, p := range bindingUpdate.NewProjections {
		s, err := projectionToParquetSchemaElement(p, b.FieldSelection.FieldConfigJsonMap[p.Field])
		if err != nil {
			return "", nil, err
		}

		ta.NewColumns = append(ta.NewColumns, existingIcebergColumn{
			Name:     s.Name,
			Nullable: true, // always true for added columns
			Type:     parquetTypeToIcebergType(s.DataType),
		})
	}

	input, err := json.Marshal(ta)
	if err != nil {
		return "", nil, err
	}

	fqn := pathToFQN(b.ResourcePath)

	return fmt.Sprintf("alter table %q", fqn), func(_ context.Context) error {
		if _, err := runIcebergctl(c.cfg, "alter-table", fqn, string(input)); err != nil {
			return fmt.Errorf("altering table %q: %w", fqn, err)
		}

		return nil
	}, nil
}

func (c *catalog) appendFiles(
	materialization string,
	tablePath []string,
	filePaths []string,
	prevCheckpoint string,
	nextCheckpoint string,
) error {
	fqn := pathToFQN(tablePath)

	b, err := runIcebergctl(
		c.cfg,
		"append-files",
		materialization,
		fqn,
		prevCheckpoint,
		nextCheckpoint,
		strings.Join(filePaths, ","),
	)
	if err != nil {
		return err
	}

	if len(b) > 0 {
		log.WithFields(log.Fields{
			"table":  fqn,
			"output": string(b),
		}).Info("append files")
	}

	return nil
}

// These functions are vestigial from the age of persisting specs in the destination.

func (c *catalog) CreateMetaTables(ctx context.Context, spec *pf.MaterializationSpec) (string, boilerplate.ActionApplyFn, error) {
	return "", nil, nil
}

func (c *catalog) LoadSpec(ctx context.Context, materialization pf.Materialization) (*pf.MaterializationSpec, error) {
	return c.lastSpec, nil
}

func (c *catalog) PutSpec(ctx context.Context, spec *pf.MaterializationSpec, version string, exists bool) (string, boilerplate.ActionApplyFn, error) {
	return "", nil, nil
}