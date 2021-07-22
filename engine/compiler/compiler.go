// Copyright 2019 Drone.IO Inc. All rights reserved.
// Use of this source code is governed by the Polyform License
// that can be found in the LICENSE file.

package compiler

import (
	"context"
	"github.com/MarkWang2/drone-runner-earthly2/engine"
	"github.com/MarkWang2/drone-runner-earthly2/engine/resource"
	"github.com/dchest/uniuri"
	"github.com/drone/runner-go/clone"
	"github.com/drone/runner-go/environ"
	"github.com/drone/runner-go/environ/provider"
	"github.com/drone/runner-go/manifest"
	"github.com/drone/runner-go/pipeline/runtime"
	"github.com/drone/runner-go/registry"
	"github.com/drone/runner-go/secret"
	"github.com/earthly/earthly/ast/spec"
	"strings"
)

// random generator function
var random = func() string {
	return "drone-" + uniuri.NewLen(20)
}

// Compiler compiles the Yaml configuration file to an
// intermediate representation optimized for simple execution.
type Compiler struct {
	// Environ provides a set of environment variables that
	// should be added to each pipeline step by default.
	Environ provider.Provider

	// Labels provides a set of labels that should be added
	// to each container by default.
	Labels map[string]string

	// NetrcCloneOnly instrucs the compiler to only inject
	// the netrc file into the clone setp.
	NetrcCloneOnly bool

	// Volumes provides a set of volumes that should be
	// mounted to each pipeline container.
	Volumes map[string]string

	// Clone overrides the default plugin image used
	// when cloning a repository.
	Clone string

	// Secret returns a named secret value that can be injected
	// into the pipeline step.
	Secret secret.Provider

	// Registry returns a list of registry credentials that can be
	// used to pull private container images.
	Registry registry.Provider

	// Mount is an optional field that overrides the default
	// workspace volume and mounts to the host path
	Mount string
}

func (c *Compiler) Compile(ctx context.Context, args runtime.CompilerArgs) runtime.Spec {
	pipeline := args.Pipeline.(*resource.Pipeline)
	targets := []spec.Target{}
	dspec := &engine.Spec{}
	os := pipeline.Platform.OS

	dspec.Root = tempdir(os)
	sourcedir := join(os, dspec.Root, "drone", "src")
	dspec.WorkingDir = sourcedir

	match := manifest.Match{
		Action:   args.Build.Action,
		Cron:     args.Build.Cron,
		Ref:      args.Build.Ref,
		Repo:     args.Repo.Slug,
		Instance: args.System.Host,
		Target:   args.Build.Deploy,
		Event:    args.Build.Event,
		Branch:   args.Build.Target,
	}

	// list the global environment variables
	globals, _ := c.Environ.List(ctx, &provider.Request{
		Build: args.Build,
		Repo:  args.Repo,
	})

	// create the default environment variables.
	envs := environ.Combine(
		provider.ToMap(
			provider.FilterUnmasked(globals),
		),
		args.Build.Params,
		pipeline.Environment,
		environ.Proxy(),
		environ.System(args.System),
		environ.Repo(args.Repo),
		environ.Build(args.Build),
		environ.Stage(args.Stage),
		environ.Link(args.Repo, args.Build, args.System),
		clone.Environ(clone.Config{
			SkipVerify: pipeline.Clone.SkipVerify,
			Trace:      pipeline.Clone.Trace,
			User: clone.User{
				Name:  args.Build.AuthorName,
				Email: args.Build.AuthorEmail,
			},
		}),
	)

	// create the workspace variables
	envs["DRONE_WORKSPACE"] = sourcedir

	// create the .netrc environment variables if not
	// explicitly disabled
	if c.NetrcCloneOnly == false {
		envs = environ.Combine(envs, environ.Netrc(args.Netrc))
	}

	// create the clone step
	if pipeline.Clone.Disable == false {
		step := createClone(pipeline)
		step.ID = random()
		step.Envs = environ.Combine(envs, step.Envs)
		step.WorkingDir = sourcedir
		step.Envs = environ.Combine(step.Envs, environ.Netrc(args.Netrc))

		rp := spec.Block{}
		imageCmd := spec.Command{Name: "FROM", Args: []string{step.Image}}
		imageSM := spec.Statement{&imageCmd, nil, nil, nil, nil}
		rp = append(rp, imageSM)

		workDirCmd := spec.Command{Name: "WORKDIR", Args: []string{sourcedir}} // todo: handle pass from drone yaml workdir step.WorkingDir
		workDirSM := spec.Statement{&workDirCmd, nil, nil, nil, nil}
		rp = append(rp, workDirSM)

		for key, value := range envs {
			cmd := spec.Command{Name: "ENV", Args: []string{key, "=", value}}
			st := spec.Statement{&cmd, nil, nil, nil, nil}
			rp = append(rp, st)
		}

		cloneSt := spec.Statement{&spec.Command{Name: "RUN", Args: []string{"sh", "/usr/local/bin/clone"}}, nil, nil, nil, nil}
		rp = append(rp, cloneSt)

		saveSt := spec.Statement{&spec.Command{Name: "SAVE ARTIFACT", Args: []string{".", "AS", "LOCAL", sourcedir}}, nil, nil, nil, nil}
		rp = append(rp, saveSt)

		target := spec.Target{step.Name, rp, nil}
		targets = append(targets, target)
		step.Target = target
		dspec.Steps = append(dspec.Steps, step)
	}

	for _, step := range pipeline.Steps {
		rp := spec.Block{}
		imageCmd := spec.Command{Name: "FROM", Args: []string{step.Image}}
		imageSM := spec.Statement{&imageCmd, nil, nil, nil, nil}
		rp = append(rp, imageSM)
		workDirCmd := spec.Command{Name: "WORKDIR", Args: []string{sourcedir}} // step.WorkingDir
		workDirSM := spec.Statement{&workDirCmd, nil, nil, nil, nil}
		rp = append(rp, workDirSM)

		for key, value := range envs {
			cmd := spec.Command{Name: "ENV", Args: []string{key, "=", value}}
			st := spec.Statement{&cmd, nil, nil, nil, nil}
			rp = append(rp, st)
		}

		for key, env := range step.Environment {
			value := env.Value
			cmd := spec.Command{Name: "ENV", Args: []string{key, "=", value}}
			st := spec.Statement{&cmd, nil, nil, nil, nil}
			rp = append(rp, st)
		}

		// done yaml add copy make drone use as dockerfile way.
		cpCmd := spec.Command{Name: "COPY", Args: []string{".", "./"}}
		cpSM := spec.Statement{&cpCmd, nil, nil, nil, nil}
		rp = append(rp, cpSM)

		for _, cmd := range step.Commands {
			cmsStr := strings.Fields(cmd)
			runSt := spec.Statement{&spec.Command{Name: "RUN", Args: cmsStr}, nil, nil, nil, nil}
			rp = append(rp, runSt)
		}

		target := spec.Target{step.Name, rp, nil}
		targets = append(targets, target)
		dstep := &engine.Step{
			Name:   step.Name,
			Target: target,
		}

		if !step.When.Match(match) {
			dstep.RunPolicy = runtime.RunNever
		}
		dspec.Steps = append(dspec.Steps, dstep)
	}

	if isGraph(dspec) == false {
		configureSerial(dspec)
	} else if pipeline.Clone.Disable == false {
		configureCloneDeps(dspec)
	} else if pipeline.Clone.Disable == true {
		removeCloneDeps(dspec)
	}

	efile := spec.Earthfile{nil, nil, targets, nil, nil}
	dspec.Earthfile = efile

	return dspec
}

// helper function attempts to find and return the named secret.
// from the secret provider.
func (c *Compiler) findSecret(ctx context.Context, args runtime.CompilerArgs, name string) (s string, ok bool) {
	if name == "" {
		return
	}

	// source secrets from the global secret provider
	// and the repository secret provider.
	provider := secret.Combine(
		args.Secret,
		c.Secret,
	)

	// TODO return an error to the caller if the provider
	// returns an error.
	found, _ := provider.Find(ctx, &secret.Request{
		Name:  name,
		Build: args.Build,
		Repo:  args.Repo,
		Conf:  args.Manifest,
	})
	if found == nil {
		return
	}
	return found.Data, true
}
