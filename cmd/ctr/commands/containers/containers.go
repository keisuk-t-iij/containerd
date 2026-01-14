/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package containers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/cmd/ctr/commands"
	"github.com/containerd/containerd/cmd/ctr/commands/run"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/dump"
	"github.com/containerd/containerd/errdefs"

	// cricontainer "github.com/containerd/cri/pkg/store/container"
	containerstore "github.com/containerd/containerd/pkg/cri/store/container"
	"github.com/containerd/log"
	"github.com/containerd/typeurl/v2"
	"github.com/urfave/cli"
)

// Command is the cli command for managing containers
var Command = cli.Command{
	Name:    "containers",
	Usage:   "Manage containers",
	Aliases: []string{"c", "container"},
	Subcommands: []cli.Command{
		createCommand,
		deleteCommand,
		infoCommand,
		listCommand,
		setLabelsCommand,
		checkpointCommand,
		restoreCommand,
		updateCommand,
	},
}

var createCommand = cli.Command{
	Name:           "create",
	Usage:          "Create container",
	ArgsUsage:      "[flags] Image|RootFS CONTAINER [COMMAND] [ARG...]",
	SkipArgReorder: true,
	Flags:          append(append(commands.SnapshotterFlags, []cli.Flag{commands.SnapshotterLabels}...), commands.ContainerFlags...),
	Action: func(context *cli.Context) error {
		var (
			id     string
			ref    string
			config = context.IsSet("config")
		)

		if config {
			id = context.Args().First()
			if context.NArg() > 1 {
				return fmt.Errorf("with spec config file, only container id should be provided: %w", errdefs.ErrInvalidArgument)
			}
		} else {
			id = context.Args().Get(1)
			ref = context.Args().First()
			if ref == "" {
				return fmt.Errorf("image ref must be provided: %w", errdefs.ErrInvalidArgument)
			}
		}
		if id == "" {
			return fmt.Errorf("container id must be provided: %w", errdefs.ErrInvalidArgument)
		}
		client, ctx, cancel, err := commands.NewClient(context)
		if err != nil {
			return err
		}
		defer cancel()
		_, err = run.NewContainer(ctx, client, context)
		if err != nil {
			return err
		}
		return nil
	},
}

var listCommand = cli.Command{
	Name:      "list",
	Aliases:   []string{"ls"},
	Usage:     "List containers",
	ArgsUsage: "[flags] [<filter>, ...]",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "quiet, q",
			Usage: "Print only the container id",
		},
	},
	Action: func(context *cli.Context) error {
		var (
			filters = context.Args()
			quiet   = context.Bool("quiet")
		)
		client, ctx, cancel, err := commands.NewClient(context)
		if err != nil {
			return err
		}
		defer cancel()
		containers, err := client.Containers(ctx, filters...)
		if err != nil {
			return err
		}
		if quiet {
			for _, c := range containers {
				fmt.Printf("%s\n", c.ID())
			}
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		fmt.Fprintln(w, "CONTAINER\tIMAGE\tRUNTIME\t")
		for _, c := range containers {
			info, err := c.Info(ctx, containerd.WithoutRefreshedMetadata)
			if err != nil {
				return err
			}
			imageName := info.Image
			if imageName == "" {
				imageName = "-"
			}
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t\n",
				c.ID(),
				imageName,
				info.Runtime.Name,
			); err != nil {
				return err
			}
		}
		return w.Flush()
	},
}

var deleteCommand = cli.Command{
	Name:      "delete",
	Usage:     "Delete one or more existing containers",
	ArgsUsage: "[flags] CONTAINER [CONTAINER, ...]",
	Aliases:   []string{"del", "remove", "rm"},
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "keep-snapshot",
			Usage: "Do not clean up snapshot with container",
		},
	},
	Action: func(context *cli.Context) error {
		var exitErr error
		client, ctx, cancel, err := commands.NewClient(context)
		if err != nil {
			return err
		}
		defer cancel()
		deleteOpts := []containerd.DeleteOpts{}
		if !context.Bool("keep-snapshot") {
			deleteOpts = append(deleteOpts, containerd.WithSnapshotCleanup)
		}

		if context.NArg() == 0 {
			return fmt.Errorf("must specify at least one container to delete: %w", errdefs.ErrInvalidArgument)
		}
		for _, arg := range context.Args() {
			if err := deleteContainer(ctx, client, arg, deleteOpts...); err != nil {
				if exitErr == nil {
					exitErr = err
				}
				log.G(ctx).WithError(err).Errorf("failed to delete container %q", arg)
			}
		}
		return exitErr
	},
}

func deleteContainer(ctx context.Context, client *containerd.Client, id string, opts ...containerd.DeleteOpts) error {
	container, err := client.LoadContainer(ctx, id)
	if err != nil {
		return err
	}
	task, err := container.Task(ctx, cio.Load)
	if err != nil {
		return container.Delete(ctx, opts...)
	}
	status, err := task.Status(ctx)
	if err != nil {
		return err
	}
	if status.Status == containerd.Stopped || status.Status == containerd.Created {
		if _, err := task.Delete(ctx); err != nil {
			return err
		}
		return container.Delete(ctx, opts...)
	}
	return fmt.Errorf("cannot delete a non stopped container: %v", status)

}

var setLabelsCommand = cli.Command{
	Name:        "label",
	Usage:       "Set and clear labels for a container",
	ArgsUsage:   "[flags] CONTAINER [<key>=<value>, ...]",
	Description: "set and clear labels for a container",
	Flags:       []cli.Flag{},
	Action: func(context *cli.Context) error {
		containerID, labels := commands.ObjectWithLabelArgs(context)
		if containerID == "" {
			return fmt.Errorf("container id must be provided: %w", errdefs.ErrInvalidArgument)
		}
		client, ctx, cancel, err := commands.NewClient(context)
		if err != nil {
			return err
		}
		defer cancel()

		container, err := client.LoadContainer(ctx, containerID)
		if err != nil {
			return err
		}

		setlabels, err := container.SetLabels(ctx, labels)
		if err != nil {
			return err
		}

		var labelStrings []string
		for k, v := range setlabels {
			labelStrings = append(labelStrings, fmt.Sprintf("%s=%s", k, v))
		}

		fmt.Println(strings.Join(labelStrings, ","))

		return nil
	},
}

var infoCommand = cli.Command{
	Name:      "info",
	Usage:     "Get info about a container",
	ArgsUsage: "CONTAINER",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "spec",
			Usage: "Only display the spec",
		},
	},
	Action: func(context *cli.Context) error {
		id := context.Args().First()
		if id == "" {
			return fmt.Errorf("container id must be provided: %w", errdefs.ErrInvalidArgument)
		}
		client, ctx, cancel, err := commands.NewClient(context)
		if err != nil {
			return err
		}
		defer cancel()
		container, err := client.LoadContainer(ctx, id)
		if err != nil {
			return err
		}
		info, err := container.Info(ctx, containerd.WithoutRefreshedMetadata)
		if err != nil {
			return err
		}
		if context.Bool("spec") {
			v, err := typeurl.UnmarshalAny(info.Spec)
			if err != nil {
				return err
			}
			commands.PrintAsJSON(v)
			return nil
		}

		if info.Spec != nil && info.Spec.GetValue() != nil {
			v, err := typeurl.UnmarshalAny(info.Spec)
			if err != nil {
				return err
			}
			commands.PrintAsJSON(struct {
				containers.Container
				Spec interface{} `json:"Spec,omitempty"`
			}{
				Container: info,
				Spec:      v,
			})
			return nil
		}
		commands.PrintAsJSON(info)
		return nil
	},
}

var updateCommand = cli.Command{
	Name:      "update",
	Usage:     "Update about a container",
	ArgsUsage: "CONTAINER",
	Flags:     []cli.Flag{},
	Action: func(context *cli.Context) error {
		id := context.Args().First()
		if id == "" {
			return fmt.Errorf("container id must be provided: %w", errdefs.ErrInvalidArgument)
		}
		client, ctx, cancel, err := commands.NewClient(context)
		if err != nil {
			return err
		}
		defer cancel()
		container, err := client.LoadContainer(ctx, id)
		if err != nil {
			return err
		}
		info, err := container.Info(ctx, containerd.WithoutRefreshedMetadata)
		if err != nil {
			return err
		}

		// hashの書き換え
		metadata, ok := info.Extensions["io.cri-containerd.container.metadata"]
		if !ok {
			return fmt.Errorf("container %q does not have CRI metadata", id)
		}
		v, err := typeurl.UnmarshalAny(metadata)
		if err != nil {
			return err
		}
		// m := v.(*cricontainer.Metadata)
		m := v.(*containerstore.Metadata)
		hash := strconv.FormatUint(HashContainer(&info), 16)
		fmt.Printf("Updating container %q hash to %s\n", id, hash)
		m.Config.Annotations["io.kubernetes.container.hash"] = hash
		newmetadata, err := typeurl.MarshalAny(m)
		if err != nil {
			return err
		}

		// Extensionの書き換え
		opt := WithContainerExtension("io.cri-containerd.container.metadata", newmetadata)
		if err := container.Update(ctx, containerd.UpdateContainerOpts(opt)); err != nil {
			return err
		}
		return nil
	},
}

// containerd.UpdateContainerOpts とほぼ同じだが、NewContainerOpsではなくUpdateContainerOptsとして実装している
func WithContainerExtension(name string, extension interface{}) containerd.UpdateContainerOpts {
	return func(ctx context.Context, client *containerd.Client, c *containers.Container) error {
		if name == "" {
			return fmt.Errorf("extension key must not be zero-length: %w", errdefs.ErrInvalidArgument)
		}

		any, err := typeurl.MarshalAny(extension)
		if err != nil {
			if errors.Is(err, typeurl.ErrNotFound) {
				return fmt.Errorf("extension %q is not registered with the typeurl package, see `typeurl.Register`: %w", name, err)
			}
			return fmt.Errorf("error marshalling extension: %w", err)
		}

		if c.Extensions == nil {
			c.Extensions = make(map[string]typeurl.Any)
		}
		c.Extensions[name] = any
		return nil
	}
}

func HashContainer(container *containers.Container) uint64 {
	hasher := fnv.New32a()
	containerJSON, _ := json.Marshal(pickFieldsToHash(container))
	hasher.Reset()
	fmt.Fprintf(hasher, "%v", dump.ForHash(containerJSON))
	return uint64(hasher.Sum32())
}

func pickFieldsToHash(container *containers.Container) map[string]string {
	// imageの先頭に"docker.io/"が含まれていたら取り除く
	image := container.Image
	if strings.HasPrefix(image, "docker.io/") {
		image = strings.TrimPrefix(image, "docker.io/")
	}

	fmt.Printf("name: %s, image: %s\n", container.Labels["io.kubernetes.container.name"], image)
	retval := map[string]string{
		"name":  container.Labels["io.kubernetes.container.name"], // pod.spec.containers[].name に対応
		"image": image,
	}
	return retval
}

func init() {
	typeurl.Register(&containerstore.Metadata{},
		"github.com/containerd/cri/pkg/store/container", "Metadata")
}
