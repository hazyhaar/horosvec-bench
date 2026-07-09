package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ipLimiter est un limiteur de débit par adresse IP, à seau de jetons (token bucket) en
// mémoire. Chaque IP dispose d'un seau qui se remplit à `rate` jetons par seconde, plafonné à
// `burst`. Une requête consomme un jeton ; sans jeton disponible, elle est refusée. La
// structure est protégée par un unique mutex (aucun accès concurrent au corps) et purgée
// périodiquement des seaux inactifs pour borner l'empreinte mémoire sous flux d'IP variées.
type ipLimiter struct {
	mu      sync.Mutex
	rate    float64 // jetons régénérés par seconde
	burst   float64 // capacité maximale du seau
	buckets map[string]*tokenBucket
	ttl     time.Duration // durée d'inactivité au-delà de laquelle un seau est purgé
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// newIPLimiter construit un limiteur (rate jetons/s, capacité burst) et lance sa purge
// périodique des seaux inactifs, arrêtée à l'annulation du contexte.
func newIPLimiter(ctx context.Context, rate, burst float64) *ipLimiter {
	l := &ipLimiter{
		rate:    rate,
		burst:   burst,
		buckets: make(map[string]*tokenBucket),
		ttl:     10 * time.Minute,
	}
	go l.janitor(ctx)
	return l
}

// allow retourne vrai si la requête de l'IP donnée est autorisée (un jeton disponible),
// faux sinon. Met à jour le seau (régénération proportionnelle au temps écoulé).
func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.buckets[ip]
	if b == nil {
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// janitor purge périodiquement les seaux inactifs depuis plus de ttl, jusqu'à l'annulation
// du contexte. La récupération protège la goroutine d'une panique inattendue.
func (l *ipLimiter) janitor(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("panique dans le janitor du limiteur de débit", "panic", p)
		}
	}()
	ticker := time.NewTicker(l.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			l.mu.Lock()
			for ip, b := range l.buckets {
				if now.Sub(b.last) > l.ttl {
					delete(l.buckets, ip)
				}
			}
			l.mu.Unlock()
		}
	}
}
