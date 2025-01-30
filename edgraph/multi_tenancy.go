//go:build oss
// +build oss

/*
 * Copyright 2025 Hypermode Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package edgraph

import "context"

type ResetPasswordInput struct {
	UserID    string
	Password  string
	Namespace uint64
}

func (s *Server) CreateNamespaceInternal(ctx context.Context, passwd string) (uint64, error) {
	return 0, nil
}

func (s *Server) DeleteNamespace(ctx context.Context, namespace uint64) error {
	return nil
}

func (s *Server) ResetPassword(ctx context.Context, ns *ResetPasswordInput) error {
	return nil
}

func createGuardianAndGroot(ctx context.Context, namespace uint64) error {
	return nil
}
