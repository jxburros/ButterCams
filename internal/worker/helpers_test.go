package worker

import "github.com/jxburros/buttercams/internal/config"

func configProtect(s string) (string, error) { return config.Protect(s) }
