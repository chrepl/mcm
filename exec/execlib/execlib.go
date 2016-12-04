// Copyright 2016 The Minimal Configuration Manager Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package execlib

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zombiezen/mcm/catalog"
	"github.com/zombiezen/mcm/internal/depgraph"
	"github.com/zombiezen/mcm/internal/system"
)

type Applier struct {
	System system.System
	Log    Logger
}

type Logger interface {
	Infof(ctx context.Context, format string, args ...interface{})
	Error(ctx context.Context, err error)
}

func (app *Applier) Apply(ctx context.Context, c catalog.Catalog) error {
	res, _ := c.Resources()
	g, err := depgraph.New(res)
	if err != nil {
		return toError(err)
	}
	if err = app.applyCatalog(ctx, g); err != nil {
		return toError(err)
	}
	return nil
}

func (app *Applier) applyCatalog(ctx context.Context, g *depgraph.Graph) error {
	ok := true
	for !g.Done() {
		ready := g.Ready()
		if len(ready) == 0 {
			return errors.New("graph not done, but has nothing to do")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		curr := ready[0]
		res := g.Resource(curr)
		app.Log.Infof(ctx, "applying: %s", formatResource(res))
		if err := errorWithResource(res, app.applyResource(ctx, res)); err == nil {
			g.Mark(curr)
		} else {
			ok = false
			app.Log.Error(ctx, toError(err).(*Error))
			skipped := g.MarkFailure(curr)
			if len(skipped) > 0 {
				skipnames := make([]string, len(skipped))
				for i := range skipnames {
					skipnames[i] = formatResource(g.Resource(skipped[i]))
				}
				app.Log.Infof(ctx, "skipping due to failure of %s: %s", formatResource(res), strings.Join(skipnames, ", "))
			}
		}
	}
	if !ok {
		return errors.New("not all resources applied cleanly")
	}
	return nil
}

func (app *Applier) applyResource(ctx context.Context, r catalog.Resource) error {
	switch r.Which() {
	case catalog.Resource_Which_noop:
		return nil
	case catalog.Resource_Which_file:
		f, err := r.File()
		if err != nil {
			return err
		}
		return app.applyFile(ctx, f)
	case catalog.Resource_Which_exec:
		e, err := r.Exec()
		if err != nil {
			return err
		}
		return app.applyExec(ctx, e)
	default:
		return errorf("unknown type %v", r.Which())
	}
}

func (app *Applier) applyFile(ctx context.Context, f catalog.File) error {
	path, err := f.Path()
	if err != nil {
		return errorf("read file path from catalog: %v", err)
	} else if path == "" {
		return errors.New("file path is empty")
	}
	switch f.Which() {
	case catalog.File_Which_plain:
		if f.Plain().HasContent() {
			content, err := f.Plain().Content()
			if err != nil {
				return errorf("read content from catalog: %v", err)
			}
			// TODO(soon): respect file mode
			if err := system.WriteFile(ctx, app.System, path, content, 0666); err != nil {
				return err
			}
		} else {
			info, err := app.System.Lstat(ctx, path)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				// TODO(soon): what kind of node it?
				return errorf("%s is not a regular file")
			}
		}
	case catalog.File_Which_directory:
		// TODO(soon): respect file mode
		if err := app.System.Mkdir(ctx, path, 0777); err == nil || !os.IsExist(err) {
			return err
		}
		// Ensure that what exists is a directory.
		info, err := app.System.Lstat(ctx, path)
		if err != nil {
			return errorf("determine state of %s: %v", path, err)
		}
		if !info.IsDir() {
			// TODO(soon): what kind of node it?
			return errorf("%s is not a directory", path)
		}
	case catalog.File_Which_symlink:
		target, err := f.Symlink().Target()
		if err != nil {
			return errorf("read target from catalog: %v", err)
		}
		if err := app.System.Symlink(ctx, target, path); err == nil || !os.IsExist(err) {
			return err
		}
		// Ensure that what exists is a symlink before trying to retarget.
		info, err := app.System.Lstat(ctx, path)
		if err != nil {
			return errorf("determine state of %s: %v", path, err)
		}
		if info.Mode()&os.ModeType != os.ModeSymlink {
			// TODO(soon): what kind of node is it?
			return errorf("%s is not a symlink", path)
		}
		actual, err := app.System.Readlink(ctx, path)
		if err != nil {
			return err
		}
		if actual == target {
			// Already the correct link.
			return nil
		}
		if err := app.System.Remove(ctx, path); err != nil {
			return errorf("retargeting %s: %v", path, err)
		}
		if err := app.System.Symlink(ctx, target, path); err != nil {
			return errorf("retargeting %s: %v", path, err)
		}
	case catalog.File_Which_absent:
		err := app.System.Remove(ctx, path)
		if err == nil || !os.IsNotExist(err) {
			return err
		}
	default:
		return errorf("unsupported file directive %v", f.Which())
	}
	return nil
}

func (app *Applier) applyExec(ctx context.Context, e catalog.Exec) error {
	switch e.Condition().Which() {
	case catalog.Exec_condition_Which_always:
		// Continue.
	case catalog.Exec_condition_Which_onlyIf:
		cond, err := e.Condition().OnlyIf()
		if err != nil {
			return errorf("condition: %v", err)
		}
		cmd, err := buildCommand(cond)
		if err != nil {
			return errorf("condition: %v", err)
		}
		out, err := app.System.Run(ctx, cmd)
		if _, exitFail := err.(*exec.ExitError); exitFail {
			return nil
		} else if err != nil {
			return errorWithOutput(out, errorf("condition: %v", err))
		}
	case catalog.Exec_condition_Which_unless:
		cond, err := e.Condition().Unless()
		if err != nil {
			return errorf("condition: %v", err)
		}
		cmd, err := buildCommand(cond)
		if err != nil {
			return errorf("condition: %v", err)
		}
		out, err := app.System.Run(ctx, cmd)
		if err == nil {
			return nil
		} else if _, exitFail := err.(*exec.ExitError); !exitFail {
			return errorWithOutput(out, errorf("condition: %v", err))
		}
	case catalog.Exec_condition_Which_fileAbsent:
		path, _ := e.Condition().FileAbsent()
		if _, err := app.System.Lstat(ctx, path); err == nil {
			// File exists; skip command.
			return nil
		} else if !os.IsNotExist(err) {
			return errorf("condition: %v", err)
		}
	default:
		return errorf("unknown condition %v", e.Condition().Which())
	}

	main, err := e.Command()
	if err != nil {
		return errorf("command: %v", err)
	}
	cmd, err := buildCommand(main)
	if err != nil {
		return errorf("command: %v", err)
	}
	out, err := app.System.Run(ctx, cmd)
	if err != nil {
		return errorWithOutput(out, errorf("command: %v", err))
	}
	return nil
}

func buildCommand(cmd catalog.Exec_Command) (*system.Cmd, error) {
	var c *system.Cmd
	switch cmd.Which() {
	case catalog.Exec_Command_Which_argv:
		argList, _ := cmd.Argv()
		if argList.Len() == 0 {
			return nil, errorf("0-length argv")
		}
		argv := make([]string, argList.Len())
		for i := range argv {
			var err error
			argv[i], err = argList.At(i)
			if err != nil {
				return nil, errorf("argv[%d]: %v", i, err)
			}
		}
		if !filepath.IsAbs(argv[0]) {
			return nil, errorf("argv[0] (%q) is not an absolute path", argv[0])
		}
		c = &system.Cmd{
			Path: argv[0],
			Args: argv,
		}
	default:
		return nil, errorf("unsupported command type %v", cmd.Which())
	}

	env, _ := cmd.Environment()
	c.Env = make([]string, env.Len())
	for i := range c.Env {
		ei := env.At(i)
		k, err := ei.NameBytes()
		if err != nil {
			return nil, errorf("getting environment[%d]: %v", i, err)
		} else if len(k) == 0 {
			return nil, errorf("environment[%d] missing name", i)
		}
		v, _ := ei.ValueBytes()
		buf := make([]byte, 0, len(k)+len(v)+1)
		buf = append(buf, k...)
		buf = append(buf, '=')
		buf = append(buf, v...)
		c.Env[i] = string(buf)
	}

	c.Dir, _ = cmd.WorkingDirectory()
	if c.Dir == "" {
		c.Dir = system.LocalRoot
	} else if !filepath.IsAbs(c.Dir) {
		return nil, errorf("working directory %q is not absolute", c.Dir)
	}

	return c, nil
}

func formatResource(r catalog.Resource) string {
	c, _ := r.Comment()
	if c == "" {
		return fmt.Sprintf("id=%d", r.ID())
	}
	return fmt.Sprintf("%s (id=%d)", c, r.ID())
}
