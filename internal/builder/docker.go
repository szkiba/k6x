// SPDX-FileCopyrightText: 2023 Iván SZKIBA
//
// SPDX-License-Identifier: AGPL-3.0-only

package builder

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/szkiba/k6x/internal/dependency"

	"github.com/docker/cli/cli/connhelper"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	builderImage = "szkiba/k6x"
	cacheVolume  = "k6x-cache"
	cachePath    = "/cache"
	workdirPath  = "/home/k6x"
)

func (b *dockerBuilder) cmdline(platform *Platform, mods dependency.Modules) ([]string, []string) {
	args := make([]string, 0, 2*len(mods))
	env := make([]string, 0, 1)

	env = append(env, "GOOS="+platform.OS)
	env = append(env, "GOARCH="+platform.Arch)

	args = append(args, "build")

	for _, mod := range mods {
		args = append(args, "--with", mod.Name+" "+mod.Tag())
	}

	return args, env
}

type dockerBuilder struct {
	cli *client.Client
}

func newDockerCLI() (*client.Client, error) {
	opts := make([]client.Opt, 0, 2)

	opts = append(opts, client.WithAPIVersionNegotiation())

	host := os.Getenv(client.EnvOverrideHost) //nolint:forbidigo
	if strings.HasPrefix(host, "ssh://") {
		helper, err := connhelper.GetConnectionHelper(host)
		if err != nil {
			return nil, err
		}

		httpClient := &http.Client{Transport: &http.Transport{DialContext: helper.Dialer}}

		opts = append(
			opts,
			client.WithHTTPClient(httpClient),
			client.WithHost(helper.Host),
			client.WithDialContext(helper.Dialer),
		)
	} else {
		opts = append(opts, client.FromEnv)
	}

	return client.NewClientWithOpts(opts...)
}

func newDockerBuilder(ctx context.Context) (Builder, bool, error) {
	cli, err := newDockerCLI()
	if err != nil {
		return nil, false, err
	}

	if _, err = cli.Ping(ctx); err != nil {
		return nil, false, nil //nolint:nilerr
	}

	return &dockerBuilder{cli: cli}, true, nil
}

func (b *dockerBuilder) close() {
	if err := b.cli.Close(); err != nil {
		logrus.Error(err)
	}
}

func (b *dockerBuilder) pull(ctx context.Context) error {
	logrus.Debugf("Pulling %s image", builderImage)

	reader, err := b.cli.ImagePull(ctx, builderImage, types.ImagePullOptions{})
	if err != nil {
		return err
	}

	defer reader.Close() //nolint:errcheck

	decoder := json.NewDecoder(reader)

	for decoder.More() {
		line := make(map[string]interface{})
		if err = decoder.Decode(&line); err != nil {
			logrus.WithError(err).Error("Error while decoding docker pull output")

			break
		}

		if _, ok := line["progress"]; ok {
			continue
		}

		if status, ok := line["status"]; ok {
			delete(line, "progressDetail")
			delete(line, "status")

			e := logrus.NewEntry(logrus.StandardLogger())
			for k, v := range line {
				e = e.WithField(k, v)
			}

			e.Debug(status)
		} else {
			logrus.Debug(line)
		}
	}

	return nil
}

func (b *dockerBuilder) start(
	ctx context.Context,
	platform *Platform,
	mods dependency.Modules,
) (string, error) {
	cmd, env := b.cmdline(platform, mods)

	logrus.Debugf("Executing %s", strings.Join(cmd, " "))

	conf := &container.Config{
		Image: builderImage,
		Cmd:   cmd,
		Tty:   false,
		Env:   env,
	}

	hconf := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: cacheVolume,
				Target: cachePath,
			},
		},
	}

	resp, err := b.cli.ContainerCreate(ctx, conf, hconf, nil, nil, "")
	if err != nil {
		return "", err
	}

	logrus.Debugf("Starting container: %s", resp.ID)
	if err = b.cli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
		return "", err
	}

	return resp.ID, nil
}

func (b *dockerBuilder) wait(ctx context.Context, id string) error {
	statusCh, errCh := b.cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
	case <-statusCh:
	}

	return nil
}

func (b *dockerBuilder) log(ctx context.Context, id string) error {
	if !logrus.IsLevelEnabled(logrus.DebugLevel) {
		return nil
	}

	var out io.ReadCloser

	out, err := b.cli.ContainerLogs(
		ctx,
		id,
		types.ContainerLogsOptions{ShowStdout: true, ShowStderr: true},
	)
	if err != nil {
		return err
	}

	lout := logrus.StandardLogger().Writer()

	_, err = stdcopy.StdCopy(lout, lout, out)

	return err
}

func (b *dockerBuilder) copy(ctx context.Context, id string, out io.Writer) error {
	binary, _, err := b.cli.CopyFromContainer(ctx, id, workdirPath)
	if err != nil {
		return err
	}

	archive := tar.NewReader(binary)

	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return err
		}

		if header.Typeflag == tar.TypeReg {
			if !strings.HasPrefix(filepath.Base(header.Name), "k6") {
				continue
			}

			_, err = io.Copy(out, archive) //nolint:gosec
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (b *dockerBuilder) Engine() Engine {
	return Docker
}

func (b *dockerBuilder) Build(
	ctx context.Context,
	platform *Platform,
	mods dependency.Modules,
	out io.Writer,
) error {
	defer b.close()

	if platform == nil {
		platform = RuntimePlatform()
	}

	return b.build(ctx, platform, mods, out)
}

func (b *dockerBuilder) build(
	ctx context.Context,
	platform *Platform,
	mods dependency.Modules,
	out io.Writer,
) error {
	logrus.Debug("Building new k6 binary (docker)")

	if err := b.pull(ctx); err != nil {
		return err
	}

	id, err := b.start(ctx, platform, mods)
	if err != nil {
		return err
	}

	defer func() {
		logrus.Debugf("Removing container: %s", id)

		rerr := b.cli.ContainerRemove(ctx, id, types.ContainerRemoveOptions{})
		if rerr != nil && err == nil {
			err = rerr
		}
	}()

	if err = b.wait(ctx, id); err != nil {
		return err
	}

	if err = b.log(ctx, id); err != nil {
		return err
	}

	if err = b.copy(ctx, id, out); err != nil {
		return err
	}

	return err
}
