package server

import (
	"context"
	"time"

	"github.com/cristalhq/base64"

	"aidanwoods.dev/go-paseto"
	"github.com/nrmnqdds/gomaluum/internal/constants"
	pb "github.com/nrmnqdds/gomaluum/internal/proto"
	"github.com/nrmnqdds/gomaluum/pkg/apikey"
)

type TokenPayload struct {
	username      string
	password      string
	imaluumCookie string
	apiKey        string
}

// imaluumSessionTTL is how long a fetched i-Ma'luum session cookie is reused
// (cached in the TokenManager) before we re-authenticate. It must stay safely
// below i-Ma'luum's own session timeout, otherwise a cached cookie can go stale
// and requests fail until the entry expires.
const imaluumSessionTTL = 30 * time.Minute

// GeneratePasetoToken generates a PASETO token for the given original uia cookie
// origin: the original uia cookie
// username: the username of the user
// password: the password of the user in base64
func (s *Server) GeneratePasetoToken(payload TokenPayload) (string, string, error) {
	logger := s.log
	token := s.paseto.Token

	token.SetIssuedAt(time.Now())
	token.SetNotBefore(time.Now())
	// token.SetExpiration(time.Now().Add(time.Minute * 30)) // 30 minutes
	token.SetExpiration(time.Now()) // now
	// token.SetExpiration(time.Now().Add(time.Minute * 1)) // 1 minutes
	token.SetIssuer("gomaluum")

	originPassword := payload.password
	imaluumCookie := payload.imaluumCookie
	username := payload.username
	userAPIKey := payload.apiKey

	// encode the base64 password
	password := []byte(originPassword)
	base64Password := base64.StdEncoding.EncodeToString(password)

	// Encrypt sensitive data with API key before storing in PASETO
	encryptedCookie, err := apikey.EncryptWithAPIKey(imaluumCookie, userAPIKey)
	if err != nil {
		logger.Error("Failed to encrypt cookie with API key", "error", err)
		return "", "", err
	}

	encryptedUsername, err := apikey.EncryptWithAPIKey(username, userAPIKey)
	if err != nil {
		logger.Error("Failed to encrypt username with API key", "error", err)
		return "", "", err
	}

	encryptedPassword, err := apikey.EncryptWithAPIKey(base64Password, userAPIKey)
	if err != nil {
		logger.Error("Failed to encrypt password with API key", "error", err)
		return "", "", err
	}

	token.SetString("imaluumCookie", encryptedCookie)
	token.SetString("username", encryptedUsername)
	token.SetString("password", encryptedPassword)

	signed := token.V4Sign(*s.paseto.PrivateKey, nil)

	s.paseto.Token = token

	return signed, imaluumCookie, nil
}

// DecodePasetoToken decodes the given PASETO token and returns the original uia cookie
func (s *Server) DecodePasetoToken(ctx context.Context, token, userAPIKey string) (*TokenPayload, error) {
	parser := paseto.NewParserWithoutExpiryCheck() // Don't use NewParser() which will checks expiry by default
	logger := s.log

	// Don't throw an error immediately if the token has expired
	// parser.AddRule(paseto.NotExpired())         // this will fail if the token has expired
	parser.AddRule(paseto.IssuedBy("gomaluum")) // this will fail if the token was not issued by "gomaluum"

	decodedToken, err := parser.ParseV4Public(*s.paseto.PublicKey, token, nil) // this will fail if parsing failes, cryptographic checks fail, or validation rules fail
	if err != nil {
		logger.ErrorContext(ctx, "Failed to parse token", "error", err)

		return nil, err
	}

	tokenExpiryDate, err := decodedToken.GetExpiration()
	if err != nil {
		logger.ErrorContext(ctx, "Failed to get expiration", "error", err)
		return nil, err
	}

	today := time.Now()

	// Get encrypted data from token
	encryptedUsername, _ := decodedToken.GetString("username")
	encryptedPassword, _ := decodedToken.GetString("password")

	// Decrypt data using API key
	username, err := apikey.DecryptWithAPIKey(encryptedUsername, userAPIKey)
	if err != nil {
		logger.ErrorContext(ctx, "Failed to decrypt username with API key", "error", err)
		return nil, err
	}

	password, err := apikey.DecryptWithAPIKey(encryptedPassword, userAPIKey)
	if err != nil {
		logger.ErrorContext(ctx, "Failed to decrypt password with API key", "error", err)
		return nil, err
	}

	// if the token has expired, we need to regenerate it
	if today.After(tokenExpiryDate) {
		logger.DebugContext(ctx, "Token has expired")

		// decode the password
		decodedPassword, err := base64.StdEncoding.DecodeString(password)
		if err != nil {
			logger.ErrorContext(ctx, "Failed to decode password", "error", err)
			return nil, err
		}

		refresh := s.sessionFunc(ctx, username, string(decodedPassword))

		newToken, err := s.tokenManager.GetToken(username, refresh)
		if err != nil {
			logger.ErrorContext(ctx, "Failed to get token", "error", err)
			return nil, err
		}

		logger.DebugContext(ctx, "Refreshed token", "username", username)

		go s.UpdateAnalytics(username)

		return &TokenPayload{
			username:      username,
			password:      string(decodedPassword),
			imaluumCookie: newToken,
			apiKey:        userAPIKey,
		}, nil

		// End of if token expired
	}

	// If token not expired yet - decrypt the cookie
	encryptedCookie, _ := decodedToken.GetString("imaluumCookie")
	imaluumCookie, err := apikey.DecryptWithAPIKey(encryptedCookie, userAPIKey)
	if err != nil {
		logger.ErrorContext(ctx, "Failed to decrypt cookie with API key", "error", err)
		return nil, err
	}

	plainPassword, err := base64.StdEncoding.DecodeString(password)
	if err != nil {
		logger.ErrorContext(ctx, "Failed to decode password", "error", err)
		return nil, err
	}

	go s.UpdateAnalytics(username)
	return &TokenPayload{
		username:      username,
		password:      string(plainPassword),
		imaluumCookie: imaluumCookie,
		apiKey:        userAPIKey,
	}, nil
}

// login performs the actual i-Ma'luum login via the GAS gRPC auth service and
// returns the fresh MOD_AUTH_CAS cookie. password must be plaintext.
func (s *Server) login(ctx context.Context, username, password string) (string, error) {
	logger := s.log
	logger.DebugContext(ctx, "Logging in to i-Ma'luum", "username", username)

	if username == constants.DebugUsername && password == constants.DebugPassword {
		logger.InfoContext(ctx, "Using fake user for debugging (login)")
		return constants.DebugUserCookie, nil
	}

	resp, err := s.grpc.client.Login(ctx, &pb.LoginRequest{
		Username: username,
		Password: password,
	})
	if err != nil {
		logger.ErrorContext(ctx, "Failed to login", "error", err)
		return "", err
	}
	return resp.Token, nil
}

// sessionFunc returns a TokenManager refresh closure that yields a live
// MOD_AUTH_CAS cookie for the user. It reuses the cookie persisted in GEI (L2)
// when present, otherwise logs in and persists the fresh cookie — so a user with
// a forever-token re-logs-in only on a GEI miss (first login or after a stale
// eviction), not on every request. The returned expiry only bounds the in-memory
// L1 (TokenManager) before it re-consults GEI; a hit there does not re-login. GEI
// failures are non-fatal — we fall back to logging in.
func (s *Server) sessionFunc(ctx context.Context, username, password string) func() (string, time.Time, error) {
	return func() (string, time.Time, error) {
		expiry := time.Now().Add(imaluumSessionTTL)

		if s.indexer != nil {
			if cookie, ok, err := s.indexer.GetSession(ctx, username); err != nil {
				s.log.WarnContext(ctx, "GEI GetSession failed, logging in", "error", err)
			} else if ok {
				s.log.DebugContext(ctx, "reusing i-Ma'luum session from GEI (skipped login)", "username", username)
				return cookie, expiry, nil
			}
		}

		cookie, err := s.login(ctx, username, password)
		if err != nil {
			return "", time.Now(), err
		}
		if s.indexer != nil {
			if err := s.indexer.StoreSession(ctx, username, cookie); err != nil {
				s.log.WarnContext(ctx, "GEI StoreSession failed", "error", err)
			}
		}
		return cookie, expiry, nil
	}
}

// refreshSession evicts the cached session for username (in-memory L1 + GEI L2)
// and forces a fresh login, returning the new cookie. Used to recover from a
// stale session.
func (s *Server) refreshSession(ctx context.Context, username, password string) (string, error) {
	s.tokenManager.Invalidate(username)
	if s.indexer != nil {
		if err := s.indexer.DeleteSession(ctx, username); err != nil {
			s.log.WarnContext(ctx, "GEI DeleteSession failed", "error", err)
		}
	}
	return s.tokenManager.GetToken(username, s.sessionFunc(ctx, username, password))
}
