package app

import (
	"testing"

	"yt-dlp-manager/internal/config"
)

// Flags beat environment beats file. A bool flag's value alone cannot say
// whether it was given, so "--allow-unauthenticated=false" used to be unable
// to switch off a true coming from the config file or the environment.
func TestBoolFlagCanOverrideToFalse(t *testing.T) {
	t.Setenv("YTDLP_MANAGER_ALLOW_UNAUTHENTICATED", "true")
	sf, _, err := parseServerFlags([]string{"--allow-unauthenticated=false"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Server.AllowUnauthenticated = true
	src := map[string]config.Source{"server.allow_unauthenticated": config.SourceFile}

	applyBool(&cfg.Server.AllowUnauthenticated, src,
		"server.allow_unauthenticated", "allow-unauthenticated",
		"YTDLP_MANAGER_ALLOW_UNAUTHENTICATED", sf, sf.allowUnauth)

	if cfg.Server.AllowUnauthenticated {
		t.Fatal("--allow-unauthenticated=false did not override the config/env true")
	}
	if src["server.allow_unauthenticated"] != config.SourceFlag {
		t.Fatalf("source = %q, want flag", src["server.allow_unauthenticated"])
	}
}

// An environment-provided value must be reported as coming from the
// environment, not mislabelled as a flag.
func TestEnvValueIsReportedAsEnv(t *testing.T) {
	t.Setenv("YTDLP_MANAGER_SECURE_COOKIE", "true")
	sf, _, err := parseServerFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	src := map[string]config.Source{"server.secure_cookie": config.SourceDefault}

	applyBool(&cfg.Server.SecureCookie, src,
		"server.secure_cookie", "secure-cookie",
		"YTDLP_MANAGER_SECURE_COOKIE", sf, sf.secureCookie)

	if !cfg.Server.SecureCookie {
		t.Fatal("environment true was not applied")
	}
	if src["server.secure_cookie"] != config.SourceEnv {
		t.Fatalf("source = %q, want env", src["server.secure_cookie"])
	}
}

// With neither flag nor environment, the file's value survives untouched.
func TestFileValueSurvivesWithoutFlagOrEnv(t *testing.T) {
	sf, _, err := parseServerFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Server.SecureCookie = true
	src := map[string]config.Source{"server.secure_cookie": config.SourceFile}

	applyBool(&cfg.Server.SecureCookie, src,
		"server.secure_cookie", "secure-cookie",
		"YTDLP_MANAGER_SECURE_COOKIE", sf, sf.secureCookie)

	if !cfg.Server.SecureCookie {
		t.Fatal("the file value was clobbered")
	}
	if src["server.secure_cookie"] != config.SourceFile {
		t.Fatalf("source = %q, want file", src["server.secure_cookie"])
	}
}
