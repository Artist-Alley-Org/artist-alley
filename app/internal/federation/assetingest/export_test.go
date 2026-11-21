// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetingest

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// SetAfterFetch runs f after every original was fetched, before the
// completing transaction. Tests only.
func SetAfterFetch(s *Service, f func()) { s.afterFetch = f }

// SetInComplete runs f inside the completing transaction, after the
// pins and the verified files are written and before the record is
// marked complete; an error rolls the transaction back. Tests only.
func SetInComplete(s *Service, f func(ctx context.Context, tx pgx.Tx) error) { s.inComplete = f }

// SetInAwait runs f inside awaitGrant's transaction, after it found no
// grant row and before it records the wait. Tests only.
func SetInAwait(s *Service, f func()) { s.inAwait = f }
