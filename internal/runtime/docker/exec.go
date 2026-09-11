package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/tmls-ai/guino/internal/runtime"
)

// Exec runs a command synchronously inside the container and returns the result.
func (r *DockerRuntime) Exec(ctx context.Context, id string, opts runtime.ExecOpts) (*runtime.ExecResult, error) {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	var env []string
	for k, v := range opts.Env {
		env = append(env, k+"="+v)
	}

	execCfg := container.ExecOptions{
		Cmd:          opts.Cmd,
		Env:          env,
		WorkingDir:   opts.WorkDir,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  opts.Stdin != nil,
	}

	containerName := r.containerName(id)

	execResp, err := r.cli.ContainerExecCreate(ctx, containerName, execCfg)
	if err != nil {
		return nil, fmt.Errorf("creating exec in %s: %w", id, err)
	}

	attachResp, err := r.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("attaching to exec in %s: %w", id, err)
	}
	defer attachResp.Close()

	if opts.Stdin != nil {
		go func() {
			_, _ = io.Copy(attachResp.Conn, opts.Stdin)
			_ = attachResp.CloseWrite()
		}()
	}

	var stdout, stderr bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, attachResp.Reader)
	if err != nil {
		return nil, fmt.Errorf("reading exec output from %s: %w", id, err)
	}

	inspectResp, err := r.cli.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return nil, fmt.Errorf("inspecting exec in %s: %w", id, err)
	}

	return &runtime.ExecResult{
		ExitCode: inspectResp.ExitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

// dockerExecStream implements runtime.ExecStream.
type dockerExecStream struct {
	ch     chan runtime.ExecStreamMessage
	done   chan struct{}
	closer io.Closer
	cancel context.CancelFunc
	once   sync.Once
	pipes  []io.Closer // pipe readers to close on Close()
}

func (s *dockerExecStream) Recv() (runtime.ExecStreamMessage, error) {
	select {
	case msg, ok := <-s.ch:
		if !ok {
			return runtime.ExecStreamMessage{}, io.EOF
		}
		return msg, nil
	case <-s.done:
		return runtime.ExecStreamMessage{}, io.EOF
	}
}

func (s *dockerExecStream) Close() error {
	s.once.Do(func() {
		close(s.done)
		for _, p := range s.pipes {
			_ = p.Close()
		}
		if s.cancel != nil {
			s.cancel()
		}
	})
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}

// ExecStream runs a command inside the container and returns a streaming interface.
func (r *DockerRuntime) ExecStream(ctx context.Context, id string, opts runtime.ExecOpts) (runtime.ExecStream, error) {
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	// NOTE: cancel is stored in the stream and called on stream.Close(),
	// not deferred here, so the context lives as long as the stream.

	var env []string
	for k, v := range opts.Env {
		env = append(env, k+"="+v)
	}

	execCfg := container.ExecOptions{
		Cmd:          opts.Cmd,
		Env:          env,
		WorkingDir:   opts.WorkDir,
		AttachStdout: true,
		AttachStderr: true,
	}

	containerName := r.containerName(id)

	execResp, err := r.cli.ContainerExecCreate(ctx, containerName, execCfg)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, fmt.Errorf("creating exec stream in %s: %w", id, err)
	}

	attachResp, err := r.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, fmt.Errorf("attaching exec stream in %s: %w", id, err)
	}

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	stream := &dockerExecStream{
		ch:     make(chan runtime.ExecStreamMessage, 64),
		done:   make(chan struct{}),
		closer: attachResp.Conn,
		cancel: cancel,
		pipes:  []io.Closer{stdoutR, stderrR},
	}

	go func() {
		defer close(stream.ch)

		go func() {
			_, _ = stdcopy.StdCopy(stdoutW, stderrW, attachResp.Reader)
			_ = stdoutW.Close()
			_ = stderrW.Close()
		}()

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			buf := make([]byte, 4096)
			for {
				n, err := stdoutR.Read(buf)
				if n > 0 {
					select {
					case stream.ch <- runtime.ExecStreamMessage{Type: "stdout", Data: string(buf[:n])}:
					case <-stream.done:
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()

		go func() {
			defer wg.Done()
			buf := make([]byte, 4096)
			for {
				n, err := stderrR.Read(buf)
				if n > 0 {
					select {
					case stream.ch <- runtime.ExecStreamMessage{Type: "stderr", Data: string(buf[:n])}:
					case <-stream.done:
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()

		wg.Wait()

		inspectResp, err := r.cli.ContainerExecInspect(ctx, execResp.ID)
		if err == nil {
			stream.ch <- runtime.ExecStreamMessage{
				Type: "exit",
				Data: fmt.Sprintf("%d", inspectResp.ExitCode),
			}
		}
	}()

	return stream, nil
}
