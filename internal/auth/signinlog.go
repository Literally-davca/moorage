package auth

import (
	"context"
	"errors"
	"strings"
)

// logSignInFailure writes refused sign-ins to the log (M18I) for CrowdSec, fail2ban and
// the admin: the IP and method always, the account name only when the account exists
// (people type passwords into the name field). Successful sign-ins are logged by
// finishLogin; other errors (database) are not refusals.
func (s *Service) logSignInFailure(ctx context.Context, method, ip, username string, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, ErrRateLimited) {
		s.log.Warn("sign-in rate limited", "ip", ip, "method", method)
		return
	}
	var refused RefusedError
	if !errors.Is(err, ErrInvalidCredentials) && !errors.As(err, &refused) && !errors.Is(err, errCeremonyExpired) {
		return
	}
	args := []any{"ip", ip, "method", method, "reason", "wrong credentials"}
	if errors.As(err, &refused) {
		args[5] = "refused"
	} else if errors.Is(err, errCeremonyExpired) {
		args[5] = "expired"
	}
	if name := strings.ToLower(strings.TrimSpace(username)); name != "" && usernameRe.MatchString(name) {
		if _, uerr := s.db.UserByName(context.WithoutCancel(ctx), name); uerr == nil {
			args = append(args, "user", name)
		}
	}
	s.log.Warn("sign-in failed", args...)
}
