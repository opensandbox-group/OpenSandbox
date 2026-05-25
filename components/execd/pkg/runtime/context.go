// Copyright 2025 Alibaba Group Holding Ltd.
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

package runtime

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"k8s.io/client-go/util/retry"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter"
	jupytersession "github.com/alibaba/opensandbox/execd/pkg/jupyter/session"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
)

// CreateContext provisions a kernel-backed session and returns its ID.
// Bash language uses Jupyter kernel like other languages; for pipe-based bash sessions use CreateBashSession (session API).
func (c *Controller) CreateContext(req *CreateContextRequest) (string, error) {
	// Create a new Jupyter session.
	var (
		client  *jupyter.Client
		session *jupytersession.Session
		err     error
	)

	err = retry.OnError(kernelWaitingBackoff, func(err error) bool {
		log.Error("failed to create session, retrying: %v", err)
		return err != nil
	}, func() error {
		client, session, err = c.createJupyterContext(*req)
		return err
	})
	if err != nil {
		return "", err
	}

	kernel := &jupyterKernel{
		kernelID: session.Kernel.ID,
		client:   client,
		language: req.Language,
	}
	c.storeJupyterKernel(session.ID, kernel)

	err = c.setWorkingDir(kernel, req)
	if err != nil {
		return "", fmt.Errorf("failed to setup working dir: %w", err)
	}

	return session.ID, nil
}

func (c *Controller) DeleteContext(session string) error {
	return c.deleteSessionAndCleanup(session)
}

func (c *Controller) GetContext(session string) (CodeContext, error) {
	kernel := c.getJupyterKernel(session)
	if kernel == nil {
		return CodeContext{}, ErrContextNotFound
	}
	return CodeContext{
		ID:       session,
		Language: kernel.language,
	}, nil
}

func (c *Controller) ListContext(language string) ([]CodeContext, error) {
	switch language {
	case Command.String(), BackgroundCommand.String(), SQL.String():
		return nil, fmt.Errorf("unsupported language context operation: %s", language)
	case "":
		return c.listAllContexts()
	default:
		return c.listLanguageContexts(Language(language))
	}
}

func (c *Controller) DeleteLanguageContext(language Language) error {
	contexts, err := c.listLanguageContexts(language)
	if err != nil {
		return err
	}

	seen := make(map[string]struct{})
	for _, context := range contexts {
		if _, ok := seen[context.ID]; ok {
			continue
		}
		seen[context.ID] = struct{}{}

		if err := c.deleteSessionAndCleanup(context.ID); err != nil {
			return fmt.Errorf("error deleting context %s: %w", context.ID, err)
		}
	}
	return nil
}

func (c *Controller) deleteSessionAndCleanup(session string) error {
	if c.getJupyterKernel(session) == nil {
		return ErrContextNotFound
	}
	if err := c.jupyterClient().DeleteSession(session); err != nil {
		return err
	}
	c.jupyterClientMap.Delete(session)
	c.deleteDefaultSessionByID(session)
	return nil
}

func (c *Controller) newContextID() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")
}

func (c *Controller) newIpynbPath(sessionID, cwd string) (string, error) {
	resolvedCwd, err := pathutil.ExpandPath(cwd)
	if err != nil {
		return "", err
	}
	if cwd != "" {
		err := os.MkdirAll(resolvedCwd, os.ModePerm)
		if err != nil {
			return "", err
		}
	}

	return filepath.Join(resolvedCwd, fmt.Sprintf("%s.ipynb", sessionID)), nil
}

// createDefaultLanguageJupyterContext prewarms a session for stateless execution.
func (c *Controller) createDefaultLanguageJupyterContext(language Language) error {
	if c.getDefaultLanguageSession(language) != "" {
		return nil
	}

	var (
		client  *jupyter.Client
		session *jupytersession.Session
		err     error
	)
	err = retry.OnError(kernelWaitingBackoff, func(err error) bool {
		log.Error("failed to create context, retrying: %v", err)
		return err != nil
	}, func() error {
		client, session, err = c.createJupyterContext(CreateContextRequest{
			Language: language,
			Cwd:      "",
		})
		return err
	})
	if err != nil {
		return err
	}

	c.setDefaultLanguageSession(language, session.ID)
	c.jupyterClientMap.Store(session.ID, &jupyterKernel{
		kernelID: session.Kernel.ID,
		client:   client,
		language: language,
	})
	return nil
}

// createJupyterContext performs the actual context creation workflow.
func (c *Controller) createJupyterContext(request CreateContextRequest) (*jupyter.Client, *jupytersession.Session, error) {
	client := c.jupyterClient()

	kernel, err := c.searchKernel(client, request.Language)
	if err != nil {
		return nil, nil, err
	}

	sessionID := c.newContextID()
	ipynb, err := c.newIpynbPath(sessionID, request.Cwd)
	if err != nil {
		return nil, nil, err
	}

	jupyterSession, err := client.CreateSession(sessionID, ipynb, kernel)
	if err != nil {
		return nil, nil, err
	}

	kernels, err := client.ListKernels()
	if err != nil {
		return nil, nil, err
	}

	found := false
	for _, k := range kernels {
		if k.ID == jupyterSession.Kernel.ID {
			found = true
			break
		}
	}
	if !found {
		return nil, nil, errors.New("kernel not found")
	}

	return client, jupyterSession, nil
}

// storeJupyterKernel caches a session -> kernel mapping.
func (c *Controller) storeJupyterKernel(sessionID string, kernel *jupyterKernel) {
	c.jupyterClientMap.Store(sessionID, kernel)
}

func (c *Controller) jupyterClient() *jupyter.Client {
	httpClient := &http.Client{
		Transport: &jupyter.AuthTransport{
			Token: c.token,
			Base:  http.DefaultTransport,
		},
	}

	return jupyter.NewClient(c.baseURL,
		jupyter.WithToken(c.token),
		jupyter.WithHTTPClient(httpClient))
}

func (c *Controller) getDefaultLanguageSession(language Language) string {
	if v, ok := c.defaultLanguageSessions.Load(language); ok {
		if session, ok := v.(string); ok {
			return session
		}
	}
	return ""
}

func (c *Controller) setDefaultLanguageSession(language Language, sessionID string) {
	c.defaultLanguageSessions.Store(language, sessionID)
}

func (c *Controller) deleteDefaultSessionByID(sessionID string) {
	c.defaultLanguageSessions.Range(func(key, value any) bool {
		if s, ok := value.(string); ok && s == sessionID {
			c.defaultLanguageSessions.Delete(key)
		}
		return true
	})
}

func (c *Controller) listAllContexts() ([]CodeContext, error) {
	contexts := make([]CodeContext, 0)
	seen := make(map[string]struct{})

	c.jupyterClientMap.Range(func(key, value any) bool {
		session, _ := key.(string)
		if kernel, ok := value.(*jupyterKernel); ok && kernel != nil {
			contexts = append(contexts, CodeContext{ID: session, Language: kernel.language})
			seen[session] = struct{}{}
		}
		return true
	})

	c.defaultLanguageSessions.Range(func(key, value any) bool {
		lang, _ := key.(Language)
		session, _ := value.(string)
		if session == "" {
			return true
		}
		// Skip if already collected from jupyterClientMap to avoid duplicates.
		if _, exists := seen[session]; exists {
			return true
		}
		contexts = append(contexts, CodeContext{ID: session, Language: lang})
		return true
	})

	return contexts, nil
}

func (c *Controller) listLanguageContexts(language Language) ([]CodeContext, error) {
	contexts := make([]CodeContext, 0)
	seen := make(map[string]struct{})

	c.jupyterClientMap.Range(func(key, value any) bool {
		session, _ := key.(string)
		if kernel, ok := value.(*jupyterKernel); ok && kernel != nil && kernel.language == language {
			contexts = append(contexts, CodeContext{ID: session, Language: language})
			seen[session] = struct{}{}
		}
		return true
	})

	if defaultContext := c.getDefaultLanguageSession(language); defaultContext != "" {
		// Skip if already collected from jupyterClientMap to avoid duplicates.
		if _, exists := seen[defaultContext]; !exists {
			contexts = append(contexts, CodeContext{ID: defaultContext, Language: language})
		}
	}

	return contexts, nil
}
