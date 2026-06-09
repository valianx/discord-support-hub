# Informe de Seguridad: M1 — Identity & authZ core (Discord Support Hub)
**Fecha:** 2026-06-08
**Agente:** security
**Modo:** focused (M1 — la columna vertebral de autenticación/autorización)
**Tipo de proyecto:** backend (Go · Gin · pgx/PostgreSQL · asynq/Valkey · discordgo)
**Estándares aplicados:** OWASP Top 10 2025, CWE Top 25 2025
**Alcance:** `internal/api/middleware`, `internal/authz`, `internal/api/handlers/agents.go`, `internal/store/postgres`, `internal/secrets`, `internal/observability`, `internal/worker/project_agent_role.go`, `internal/config`, `cmd/keygen`, `cmd/api`, `migrations/0001_init.sql`, `docs/02-architecture.md`.

---

## Resumen Ejecutivo

### Veredicto de riesgo global: **MEDIO** — sin Critical, sin High que bloqueen entrega.

La columna de autenticación/autorización de M1 está **bien construida en lo fundamental**: Layer A falla-cerrado de forma demostrable (error de store ⇒ 500, nunca autoriza), las claves se comparan por igualdad de hash en la base de datos (sin comparación en memoria no-constante), el raw key nunca se registra ni se persiste, todo el SQL nuevo de pgx está parametrizado, AES-256-GCM está correctamente implementado, y la redacción de logs está cableada en el arranque. La invariante NFR-13 (authZ resuelve contra Postgres, nunca contra el rol de Discord) se respeta tanto en Layer B como en el worker de proyección de rol.

El hallazgo más relevante **no es una brecha de seguridad sino lo contrario**: el gate Admin de `POST/DELETE/GET /agents` es *inalcanzable* en producción porque `Principal.IsAdmin` se inyecta siempre como `false` y nada lo enriquece — el modelo de datos `api_keys` ni siquiera tiene una columna que vincule la clave a un usuario admin. Esto es **fallo-cerrado** (deniega de más, no de menos): no hay escalación de privilegios ni bypass posible, pero la funcionalidad de gestión de roster queda muerta. Lo clasifico como **Medio** (defecto de disponibilidad con una trampa de seguridad latente: el "arreglo" obvio e incorrecto sería confiar en un campo controlable por el cliente).

El resto son endurecimientos: el `state` OAuth sin firmar (correctamente diferido a M3, sin superficie viva porque el callback es 501), la falta de cabeceras de seguridad HTTP, ausencia de rate-limiting en el borde, y un par de observaciones menores sobre el prefijo de DSN en logs y el truncado de nonce en `Decrypt`.

**Nada en M1 expone una ruta de escalación de privilegios, auth-bypass, inyección o fuga de secretos.** Layer A y Layer B son seguros tal como están. Los Medium deben resolverse antes de que M3 active el callback OAuth y antes del primer despliegue expuesto a internet.

### Hallazgos más urgentes
1. **SEC-001 (Medio):** El gate Admin es inalcanzable en producción — `Principal.IsAdmin` siempre `false`; no hay binding clave→admin en el esquema. Riesgo latente: el arreglo incorrecto introduce un bypass.
2. **SEC-002 (Medio):** `state=userID` OAuth sin firmar — gate obligatorio de M3: el callback DEBE rechazar state sin firma/forjado/replay antes de canjear el code (CSRF). Sin superficie viva hoy (callback 501).
3. **SEC-003 (Medio):** Ausencia de cabeceras de seguridad HTTP (HSTS, CSP, X-Content-Type-Options, etc.) y de rate-limiting en endpoints de autenticación.

---

## Estadísticas de Hallazgos

| Categoría OWASP | Crítico | Alto | Medio | Bajo | Info | Total |
|-----------------|---------|------|-------|------|------|-------|
| A01 Broken Access Control | 0 | 0 | 1 | 0 | 0 | 1 |
| A02 Security Misconfiguration | 0 | 0 | 1 | 1 | 1 | 3 |
| A04 Cryptographic Failures | 0 | 0 | 0 | 0 | 1 | 1 |
| A06 Insecure Design | 0 | 0 | 0 | 1 | 0 | 1 |
| A07 Authentication Failures | 0 | 0 | 1 | 0 | 0 | 1 |
| A09 Logging Failures | 0 | 0 | 0 | 1 | 0 | 1 |
| **Total** | **0** | **0** | **3** | **3** | **3** | **9** |

---

## Validación de los focos solicitados

Antes de los hallazgos, el resultado punto por punto del checklist del encargo (lo que **pasó** la auditoría):

| Foco | Resultado | Evidencia |
|------|-----------|-----------|
| **Layer A — fail-closed en error de store** | ✅ Correcto | `middleware.go:94-108` — `err != nil` que NO es `ErrNotFound` ⇒ 500 + abort; nunca cae a `c.Next()`. Test `TestAuth_StoreError_Returns500`. |
| **Layer A — claves revocadas/inactivas rechazadas** | ✅ Correcto | `postgres.go:250` el lookup filtra `revoked_at IS NULL`; `ErrNotFound` ⇒ 401. Test `TestAuth_RevokedKey_Returns401`. |
| **Layer A — comparación constante / sin compare en memoria** | ✅ Correcto | La comparación es por igualdad de hash en el índice de Postgres (`WHERE key_hash = $1`, `postgres.go:250`), no un `bytes.Equal`/`==` en memoria sobre el secreto. SHA-256 sobre clave de 256 bits de alta entropía ⇒ sin riesgo de timing/enumeración explotable. |
| **Layer A — raw key nunca registrado** | ✅ Correcto | Único log en la ruta auth es `key_id` (UUID), `middleware.go:113`. Grep de logs no halló ningún `rawKey`/`token`/`Authorization` registrado. |
| **Layer A — 401 antes de cualquier handler** | ✅ Correcto | `abortUnauthorized` usa `AbortWithStatusJSON` (`middleware.go:168`); el grupo `/v1` monta `Auth` como primer middleware (`router.go:62`). Test `TestAuth_HandlerNotCalledOnFailure`. |
| **Layer B — decide contra Postgres, no contra el rol Discord** | ✅ Correcto | `RequireAdmin` lee solo `Principal.IsAdmin` (`authz.go:64-69`), resuelto de `users.is_admin`. Test `TestAddAgent_AuthZPurePostgres_CollaboratorDenied`. |
| **Layer B — gate Admin no bypasseable por no-admin/no-autenticado** | ✅ Correcto (sobre-restrictivo, ver SEC-001) | `nil` principal ⇒ Deny (`authz.go:65-67`); `IsAdmin=false` ⇒ 403. Tests `TestListAgents_NilPrincipal_Returns403`, `TestAddAgent_NonAdmin_Returns403`. |
| **Secrets — keygen muestra raw una vez, guarda solo hash** | ✅ Correcto | `cmd/keygen/main.go:85-107` — `HashAPIKey` al store; raw solo a stdout una vez. |
| **Secrets — encryption key + bot token solo de env** | ✅ Correcto | `config.go:64,71`; `discord.New` recibe el token por parámetro, nunca lee env adentro (`discord.go:41-42`). |
| **Secrets — AES-GCM para oauth_tokens** | ✅ Correcto | `secrets.go:62-86` — `cipher.NewGCM` + nonce aleatorio de `crypto/rand`, sello autenticado. (La escritura real es M3; la primitiva ya es correcta.) |
| **SQL — pgx parametrizado, sin inyección** | ✅ Correcto | Todas las queries en `postgres.go` usan `$1..$N`. `ListAgents`/`ListAPIKeys` concatenan solo literales constantes (`AND is_active = TRUE`), nunca input. |
| **Role mgmt — MANAGE_ROLES reservado al bot, rol no auto-asignable** | ✅ Correcto | Solo el bot llama `GuildMemberRoleAdd/Remove` (`discord.go:74,83`); no hay endpoint que permita auto-asignación. |
| **Role mgmt — proyección gateada a type=agent en Postgres** | ✅ Correcto | `project_agent_role.go:72-76` — `user.Type != UserTypeAgent` ⇒ skip, aunque exista el rol en Discord (NFR-13). |

---

## Hallazgos Detallados

### MEDIO

#### SEC-001: El gate Admin es estructuralmente inalcanzable — `Principal.IsAdmin` nunca se puebla (fail-closed con trampa latente)
- **Severidad:** Medio
- **Categoría OWASP:** A01:2025 — Broken Access Control / A07:2025 — Authentication Failures
- **CWE:** CWE-863 — Incorrect Authorization (deniega de más); riesgo latente CWE-639 — Authorization Bypass via User-Controlled Key
- **Archivos:**
  - `internal/api/middleware/middleware.go` — líneas 117-124
  - `internal/authz/authz.go` — líneas 64-69
  - `migrations/0001_init.sql` — líneas 234-247 (tabla `api_keys` sin columna de binding)
- **Descripción:** El middleware Layer A construye el `Principal` con `IsAdmin` por defecto en `false` y el comentario dice *"Handler-level enrichment (e.g. user lookup) can set it if needed"* — pero **ningún handler hace ese enrichment**. `RequireAdmin` lee exclusivamente `Principal.IsAdmin`. El resultado: en producción, con autenticación real, `RequireAdmin` devuelve **siempre `Deny`**, y por tanto `GET/POST/DELETE /agents` devuelven **siempre 403**. La gestión de roster queda muerta. Los tests que verifican el 201/200 (`TestAddAgent_Admin_*`, `TestListAgents_Admin_*`) **inyectan el principal admin directamente, bypaseando Layer A** (`agents_test.go:146-151,166`), por lo que el defecto no aflora en la suite.

  La raíz es de diseño: el esquema `api_keys` (`0001_init.sql:234-247`) **no tiene columna `user_id` ni `is_admin`** — no existe forma de vincular una clave de servicio a un usuario admin de Postgres. La arquitectura (`§5.1`) describe el principal de servicio como *backoffice full control-plane*, pero `§5.2` exige Admin (`is_admin=true`) para `/agents`. Hay una **contradicción de diseño** entre "la clave backoffice es el plano de control completo" y "Admin se decide por `users.is_admin`": la clave de servicio no es un usuario, no tiene `is_admin`, y nada cierra ese hueco.
- **Evidencia:**
  ```go
  // middleware.go:117-124
  p := &authz.Principal{
      Type:     authz.PrincipalTypeService,
      KeyID:    apiKey.ID,
      KeyScope: apiKey.Scope,
      // IsAdmin defaults false for service keys not bound to an admin user.
      // Handler-level enrichment (e.g. user lookup) can set it if needed.   <-- nadie lo hace
  }
  ```
  ```go
  // authz.go:64-69
  func RequireAdmin(p *Principal) bool {
      if p == nil { return Deny }
      return p.IsAdmin   // siempre false para principals de servicio en producción
  }
  ```
- **Impacto de seguridad:** En el estado actual, **ninguno negativo** — el sistema falla-cerrado: deniega acceso que debería conceder (problema de disponibilidad), no concede acceso que debería denegar. **No hay escalación ni bypass.** El riesgo es la **trampa latente**: el arreglo intuitivo y *equivocado* es derivar `IsAdmin` de algo controlable por el cliente (un header, un claim del body, el `scope` de la clave tratado como "scope=backoffice ⇒ admin"). Cualquiera de esos convierte un fail-closed benigno en un **auth-bypass real (CWE-639)**: quien posea *cualquier* clave de servicio válida (incluida una futura clave de scope reducido) obtendría Admin. La remediación debe anclar la decisión en estado de Postgres no influenciable por el llamante.
- **Remediación:**
  Elegir una de dos rutas, ambas ancladas en Postgres:

  **Opción A — binding de clave a usuario (recomendada, alinea esquema con `§5.2`):** añadir `api_keys.user_id UUID REFERENCES users(id)` (nullable: una clave puede ser de servicio puro). En el lookup, traer el `is_admin` del usuario vinculado y poblar el `Principal`:
  ```sql
  -- nueva migración
  ALTER TABLE api_keys ADD COLUMN user_id UUID REFERENCES users(id);
  ```
  ```go
  // LookupActiveAPIKeyByHash devuelve también el is_admin del usuario vinculado (LEFT JOIN users)
  // y el middleware lo copia a Principal.IsAdmin / Principal.UserID.
  ```
  **Opción B — el scope decide, explícitamente y solo del lado servidor:** si el modelo es "la clave backoffice ES admin del plano de control", hacerlo explícito y auditable en Layer B en lugar de dejar `IsAdmin` muerto. Definir `RequireAdmin` para aceptar también un scope administrativo resuelto del campo `api_keys.scope` (valor del servidor, no del cliente):
  ```go
  func RequireAdmin(p *Principal) bool {
      if p == nil { return Deny }
      return p.IsAdmin || p.KeyScope == "backoffice"   // scope viene de la DB, no del request
  }
  ```
  Sea cual sea la ruta, **prohibir** derivar admin de cualquier dato del request (headers, body, query). Añadir un test que ejercite el camino real (Layer A + Layer B juntos) con una clave de servicio realista para que el defecto no quede oculto tras la inyección directa del principal. Actualizar `docs/02-architecture.md §5.1/§5.2` para eliminar la contradicción servicio-vs-admin.

---

#### SEC-002: `state` de OAuth2 construido sin firma (CSRF) — gate obligatorio de M3
- **Severidad:** Medio
- **Categoría OWASP:** A07:2025 — Authentication Failures (CSRF en el flujo de authorization code)
- **CWE:** CWE-352 — Cross-Site Request Forgery; CWE-1275 (uso inseguro de parámetro de estado)
- **Archivos:**
  - `internal/api/handlers/agents.go` — líneas 236-247 (`buildDiscordOAuthURL`)
  - `internal/api/handlers/transversal.go` — líneas 18-24 (`OAuthDiscordCallback`, hoy 501)
  - `internal/oauth/oauth.go` — líneas 19-28 (`StateManager`, sin implementar)
- **Descripción:** El `connect_url` se construye con `state=<userID>` en texto plano, sin firma ni componente single-use (`agents.go:244-246`), con un `TODO(M3)` explícito para firmarlo con HMAC. El `state` OAuth2 es el mecanismo anti-CSRF del flujo de authorization code: un `state` predecible/forjable permite a un atacante vincular su propia cuenta de Discord a la sesión OAuth de la víctima (account-linking CSRF), o reproducir un `state` capturado.
- **Clasificación dado el estado actual:** **Sin superficie viva.** El callback que *valida* y *consume* el `state` (`OAuthDiscordCallback`) es M3 y hoy devuelve **501** (`transversal.go:23`, confirmado por `TestStubHandlers_TransversalEndpoints`). No hay canje de code ni validación de state ejecutándose. Por eso NO es Critical/High hoy: no bloquea la entrega de M1. Pero es un **requisito de seguridad obligatorio y bloqueante para M3** — debe quedar registrado para que no se active el callback sin él.
- **Evidencia:**
  ```go
  // agents.go:239-247
  func buildDiscordOAuthURL(clientID, redirectURL, stateUserID string) string {
      if clientID == "" || redirectURL == "" { return "" }
      return fmt.Sprintf(
          "https://discord.com/api/oauth2/authorize?client_id=%s&redirect_uri=%s&response_type=code&scope=identify%%20guilds.join&state=%s",
          clientID, redirectURL, stateUserID,   // <-- state = userID, sin firma, sin nonce
      )
  }
  ```
- **Impacto:** Nulo en M1 (callback muerto). En M3, si el callback se activara aceptando este `state` sin verificación: account-linking CSRF y replay — un atacante podría asociar un `discord_user_id` que controla a un `userID` de agente de la víctima, o re-disparar un onboarding.
- **Remediación (gate de M3, antes de que `OAuthDiscordCallback` deje de ser 501):**
  1. Implementar `StateManager.Issue` con **HMAC-SHA256** sobre `(userID, nonce, expiry)` con un secreto de servidor (env, junto a `ENCRYPTION_KEY`), y `Validate` que verifique la firma **en tiempo constante** (`hmac.Equal`), compruebe expiración y consuma el `state` como **single-use** (registro en Postgres/Valkey con TTL — rechazar replay).
  2. El callback DEBE: rechazar `state` ausente/forjado/expirado/ya-consumido **antes** de canjear el code; validar que el `redirect_uri` coincide exactamente con el configurado; nunca canjear el code si la validación de state falla.
  3. URL-encodear los parámetros (ver SEC-007).
  4. Añadir tests de M3: state forjado ⇒ rechazo; state reutilizado ⇒ rechazo; state expirado ⇒ rechazo; happy-path consume el state.

---

#### SEC-003: Ausencia de cabeceras de seguridad HTTP y de rate-limiting en el borde
- **Severidad:** Medio
- **Categoría OWASP:** A02:2025 — Security Misconfiguration
- **CWE:** CWE-693 — Protection Mechanism Failure; CWE-770 — Allocation of Resources Without Limits (login/auth sin throttling)
- **Archivos:**
  - `internal/api/router.go` — líneas 38-74 (cadena de middleware: solo Recovery, RequestID, CORS)
  - `internal/api/middleware/middleware.go` — `Auth` sin rate-limit por clave/IP
- **Descripción:** El router monta `Recovery`, `RequestID` y `corsMiddleware`, pero **ninguna cabecera de seguridad de respuesta**: falta `Strict-Transport-Security`, `X-Content-Type-Options: nosniff`, `X-Frame-Options`/`frame-ancestors`, `Referrer-Policy`, `Permissions-Policy`. Aunque la API es JSON M2M, el `connect_url` se entrega a navegadores y la POC-FE (`§5.3`) consumirá esta API desde un browser. Adicionalmente, el endpoint de autenticación (`Auth` sobre todo `/v1`) **no tiene rate-limiting** por clave o IP: un atacante puede martillar el lookup de claves sin coste (enumeración de claves de baja probabilidad pero también DoS sobre Postgres y, vía la goroutine `TouchAPIKeyLastUsed`, escrituras). El bucket distribuido de `internal/ratelimit` existe pero es para Discord (worker), no para el borde HTTP.
- **Evidencia:**
  ```go
  // router.go:42-44 — toda la cadena global de middleware
  r.Use(middleware.Recovery())
  r.Use(middleware.RequestID())
  r.Use(corsMiddleware(cfg.CORSAllowedOrigins))
  // (sin secure-headers, sin rate-limit)
  ```
- **Impacto:** Sin HSTS, un MITM puede degradar a HTTP. Sin `nosniff`/`frame-ancestors`, la futura POC-FE queda expuesta a MIME-sniffing y clickjacking. Sin rate-limit en `/v1`, un atacante autenticado-fallido puede saturar el lookup de Postgres (DoS) y disparar escrituras `last_used_at` sin control.
- **Remediación:**
  1. Añadir un middleware de cabeceras de seguridad en la cadena global de `NewRouter`:
     ```go
     r.Use(func(c *gin.Context) {
         c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
         c.Header("X-Content-Type-Options", "nosniff")
         c.Header("X-Frame-Options", "DENY")
         c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
         c.Header("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
         c.Next()
     })
     ```
  2. Añadir rate-limiting al borde (token bucket por IP y por `key_id`) sobre `/v1`, reutilizando `internal/ratelimit` sobre Valkey o un limiter en memoria por instancia. Endurecer especialmente la ruta de auth fallida.
  3. Considerar mover `TouchAPIKeyLastUsed` a un flush periódico/muestreado en lugar de una goroutine por request (evita amplificación de escritura bajo ráfaga).

---

### BAJO

#### SEC-004: `Recovery` registrado antes que `RequestID` — el `request_id` puede faltar en el log de pánico
- **Severidad:** Bajo
- **Categoría OWASP:** A09:2025 — Security Logging and Alerting Failures
- **CWE:** CWE-778 — Insufficient Logging
- **Archivo:** `internal/api/router.go` — líneas 42-43
- **Descripción:** `Recovery()` se monta antes que `RequestID()`. Si un panic ocurre dentro de `RequestID` o antes de que asigne el id, el handler de recovery lee `c.Get(requestIDKey)` vacío (`middleware.go:54`) y el log de pánico — el más importante para forense — sale sin correlación. El comentario en `router.go:41` ("recovery first so panics in other middleware are caught") justifica el orden, pero el coste es la pérdida del `request_id` en ese caso límite.
- **Evidencia:**
  ```go
  // router.go:42-43
  r.Use(middleware.Recovery())   // se monta primero
  r.Use(middleware.RequestID())  // el id se asigna después
  ```
- **Impacto:** Bajo — pérdida de correlación de logs solo en pánicos muy tempranos. No explotable.
- **Remediación:** Asignar el `request_id` lo antes posible (idealmente generarlo dentro de `Recovery` si aún no existe, o invertir el orden y envolver `RequestID` en su propio defer). Como mínimo, en el handler de recovery, generar un id efímero cuando `c.Get(requestIDKey)` esté vacío para que ningún log de pánico salga sin identificador.

---

#### SEC-005: `RequireEncryptionKey` no se valida en el arranque de `cmd/api`
- **Severidad:** Bajo (hoy; sube a Medio en M3)
- **Categoría OWASP:** A02:2025 — Security Misconfiguration
- **CWE:** CWE-665 — Improper Initialization
- **Archivos:**
  - `cmd/api/main.go` — líneas 34-41 (solo valida DSN y bot token)
  - `internal/config/config.go` — líneas 117-123 (`RequireEncryptionKey` existe pero no se invoca en `cmd/api`)
- **Descripción:** `cmd/api` valida `RequirePostgresDSN` y `RequireDiscordToken` al boot, pero **no** `RequireEncryptionKey`. La clave de cifrado AES-256-GCM (`ENCRYPTION_KEY`) solo se necesita cuando M3 escriba `oauth_tokens`, así que hoy su ausencia no rompe nada. Pero el patrón "fail loudly at startup" (declarado en `cmd/api/main.go:27` y en `.env.example:3`) no se cumple para este secreto: el servicio arrancaría sin la clave y solo fallaría en runtime al primer canje OAuth de M3, potencialmente dejando un onboarding a medias.
- **Evidencia:**
  ```go
  // cmd/api/main.go:34-41 — falta RequireEncryptionKey()
  if err = cfg.RequirePostgresDSN(); err != nil { ... os.Exit(1) }
  if err = cfg.RequireDiscordToken(); err != nil { ... os.Exit(1) }
  // (RequireEncryptionKey no se llama)
  ```
- **Impacto:** Bajo hoy (no se usa la clave en M1). En M3 sería un fallo en runtime en plena ruta de cifrado de tokens.
- **Remediación:** Cuando M3 active la escritura de `oauth_tokens`, añadir `cfg.RequireEncryptionKey()` (y validar que decodifica a exactamente 32 bytes vía `secrets.NewEncrypter` al boot) en la secuencia de arranque de `cmd/api`. Idealmente construir el `Encrypter` una vez al boot y fallar ruidoso si la clave es inválida, en lugar de descubrirlo en el primer request.

---

#### SEC-006: `Decrypt` usa `len(Ciphertext)` para validar el nonce y no comprueba `len(Nonce)`
- **Severidad:** Bajo
- **Categoría OWASP:** A04:2025 — Cryptographic Failures
- **CWE:** CWE-20 — Improper Input Validation
- **Archivo:** `internal/secrets/secrets.go` — líneas 90-110
- **Descripción:** En `Decrypt`, la guarda de longitud compara `len(ev.Ciphertext) < gcm.NonceSize()` (`secrets.go:101`), pero el nonce se almacena **separado** del ciphertext (por diseño, `secrets.go:63`). La comprobación correcta para este esquema sería validar `len(ev.Nonce) == gcm.NonceSize()`. Si una fila de `oauth_tokens` tuviera un nonce corrupto/truncado (longitud incorrecta), `gcm.Open` recibiría un nonce de tamaño inválido y haría `panic` en lugar de devolver un error limpio. El ciphertext sí lo valida GCM internamente vía el tag de autenticación, así que la guarda actual sobre el ciphertext es redundante; la que falta es la del nonce.
- **Evidencia:**
  ```go
  // secrets.go:101-105
  if len(ev.Ciphertext) < gcm.NonceSize() {   // valida el ciphertext, no el nonce
      return nil, ErrCiphertextTooShort
  }
  plaintext, err := gcm.Open(nil, ev.Nonce, ev.Ciphertext, nil)  // panic si len(ev.Nonce) != NonceSize
  ```
- **Impacto:** Bajo — requiere una fila con nonce de longitud inválida (corrupción de DB o bug de escritura), no input directo de atacante. El efecto es un `panic` (recuperado por el worker/API recovery) en lugar de un error manejado. No es explotable remotamente en M1 (no hay escritura/lectura de tokens aún).
- **Remediación:**
  ```go
  if len(ev.Nonce) != gcm.NonceSize() {
      return nil, fmt.Errorf("secrets: invalid nonce length: got %d, want %d", len(ev.Nonce), gcm.NonceSize())
  }
  ```
  Reemplazar (o complementar) la guarda actual por la validación de `len(ev.Nonce)` antes de `gcm.Open`.

---

### INFO

#### SEC-007: Parámetros de la URL OAuth interpolados sin `url.QueryEscape`
- **Severidad:** Info
- **Categoría OWASP:** A03 (calidad de construcción) — no explotable como open-redirect en M1
- **CWE:** CWE-116 — Improper Encoding or Escaping of Output
- **Archivo:** `internal/api/handlers/agents.go` — líneas 243-246
- **Descripción:** `buildDiscordOAuthURL` interpola `clientID`, `redirectURL` y `stateUserID` directamente con `fmt.Sprintf` sin `url.QueryEscape`. Hoy los tres provienen de fuentes confiables del servidor: `clientID` y `redirectURL` de config (`§5.1`, no input de usuario, por lo que **no hay open-redirect** vía `redirect_uri`), y `stateUserID` es un UUID generado por la DB. Por eso es Info y no un hallazgo de inyección. Pero cuando M3 cambie `state` a un token HMAC (base64/hex con posibles `+`, `/`, `=`), la falta de encoding podría romper o malformar la URL.
- **Remediación:** Usar `net/url` para construir la URL de forma segura:
  ```go
  u, _ := url.Parse("https://discord.com/api/oauth2/authorize")
  q := u.Query()
  q.Set("client_id", clientID)
  q.Set("redirect_uri", redirectURL)
  q.Set("response_type", "code")
  q.Set("scope", "identify guilds.join")
  q.Set("state", state)
  u.RawQuery = q.Encode()
  return u.String()
  ```

#### SEC-008: `safeDSNPrefix` registra hasta 40 caracteres del DSN — puede filtrar usuario/host
- **Severidad:** Info
- **Categoría OWASP:** A09:2025 — Logging Failures
- **CWE:** CWE-532 — Insertion of Sensitive Information into Log File
- **Archivo:** `internal/store/postgres/postgres.go` — líneas 39, 380-386
- **Descripción:** `safeDSNPrefix` corta a 40 caracteres para no exponer la contraseña embebida en el DSN. Pero un DSN típico (`postgres://hub:change-me@localhost:5432/...`, ver `.env.example:13`) **incluye usuario y contraseña dentro de los primeros 40 caracteres**: `postgres://hub:change-me@localhost:5432/disc` (44 chars) — el corte a 40 deja `postgres://hub:change-me@localhost:5432/d`, exponiendo usuario **y contraseña** en el log de arranque. La heurística de "primeros N caracteres" no es segura para credenciales embebidas en URI.
- **Evidencia:**
  ```go
  // postgres.go:380-386
  func safeDSNPrefix(dsn string) string {
      n := len(dsn)
      if n > 40 { n = 40 }
      return dsn[:n]   // los primeros 40 chars de un DSN suelen incluir user:pass@host
  }
  ```
- **Remediación:** Parsear el DSN con `net/url` (o `pgx`/`pgconn.ParseConfig`) y registrar solo componentes no sensibles (host, puerto, nombre de base), nunca el userinfo:
  ```go
  if u, err := url.Parse(dsn); err == nil {
      return fmt.Sprintf("%s/%s", u.Host, strings.TrimPrefix(u.Path, "/"))  // host:port/dbname, sin user:pass
  }
  ```

#### SEC-009: `noopAuth` fallback existe para tests — verificar que nunca alcance producción
- **Severidad:** Info
- **Categoría OWASP:** A02:2025 — Security Misconfiguration
- **CWE:** CWE-489 — Active Debug/Test Code
- **Archivos:** `internal/api/router.go` — líneas 55-60, 126-129
- **Descripción:** `NewRouter` cae a `noopAuth()` (pass-through sin autenticación) cuando `cfg.Store == nil`. Hoy es **seguro**: `cmd/api/main.go:46-51` siempre construye un `pg` real y aborta el proceso (`os.Exit(1)`) si la conexión falla, por lo que `Store` nunca es `nil` en producción. Lo registro como Info defensivo: el patrón "Store nil ⇒ sin auth" es una fragilidad latente — un refactor futuro que tolere `Store == nil` (p. ej. un modo degradado) abriría toda la API sin autenticación silenciosamente.
- **Evidencia:**
  ```go
  // router.go:55-60
  if cfg.Store != nil {
      authMiddleware = middleware.Auth(cfg.Store)
  } else {
      authMiddleware = noopAuth()   // pass-through: SIN autenticación
  }
  ```
- **Remediación:** Considerar un flag explícito (`cfg.DisableAuth bool`, default false, solo activable en test) en lugar de inferir "sin auth" de `Store == nil`. Alternativamente, hacer que `NewRouter` haga `panic`/error si `Store == nil` sin un flag de test explícito, de modo que la ausencia de auth nunca sea un efecto colateral silencioso de un Store ausente.

---

## Configuración de Seguridad

### Headers HTTP
| Header | Estado | Recomendación |
|--------|--------|---------------|
| Strict-Transport-Security | **Ausente** | `max-age=31536000; includeSubDomains` (SEC-003) |
| Content-Security-Policy | Ausente | Relevante para POC-FE; definir al construirla |
| X-Content-Type-Options | **Ausente** | `nosniff` (SEC-003) |
| X-Frame-Options | **Ausente** | `DENY` (SEC-003) |
| Referrer-Policy | Ausente | `strict-origin-when-cross-origin` (SEC-003) |
| Permissions-Policy | Ausente | Política restrictiva (SEC-003) |

### CORS
| Aspecto | Estado | Detalle |
|---------|--------|---------|
| Origins permitidos | ✅ Restrictivo | Allowlist desde config; lista vacía ⇒ sin CORS (`router.go:110-114`). Correcto. |
| Credentials con wildcard | ✅ Correcto | `AllowCredentials: false` siempre (`router.go:121`); nunca `*` con credenciales. Correcto. |

### Autenticación
| Aspecto | Estado | Detalle |
|---------|--------|---------|
| Algoritmo de hash de clave | ✅ SHA-256 sobre clave de 256 bits | Aceptable para claves de alta entropía (`authz.go:80-98`). Comparación por igualdad en índice de DB, no en memoria. |
| Raw key en logs / persistencia | ✅ Nunca | Solo `key_id` (UUID) se registra; raw solo a stdout en keygen una vez. |
| Fail-closed en error de store | ✅ Correcto | 500 + abort, nunca autoriza (`middleware.go:99-107`). |
| Revocación | ✅ Instantánea | `revoked_at` + filtro en lookup (`postgres.go:250`). |
| Cifrado de tokens en reposo | ✅ AES-256-GCM | Primitiva correcta (`secrets.go`); escritura real en M3. |
| Redacción de logs | ✅ Cableada | `redactingHandler` en `InitLogger`, activo en api/worker/keygen. |
| Binding clave→admin | **Roto** | No existe en el esquema; gate Admin inalcanzable (SEC-001). |
| CSRF state OAuth | **Diferido a M3** | Sin firma hoy; callback 501, sin superficie viva (SEC-002). |

---

## Plan de Remediación Priorizado

### Fase 1 — Inmediato (bloquear deploy)
**Ninguno.** No hay hallazgos Critical/High. M1 puede entregarse desde la óptica de seguridad de la columna de autenticación/autorización.

### Fase 2 — Antes del primer despliegue expuesto / antes de activar M3
1. **SEC-001** — Reparar el binding clave→admin (Opción A con columna `api_keys.user_id`, o Opción B con scope explícito del servidor). Añadir test end-to-end Layer A+B. **Prohibir** derivar admin de input del cliente.
2. **SEC-003** — Añadir middleware de cabeceras de seguridad y rate-limiting en el borde sobre `/v1`.
3. **SEC-002** — *Gate de M3:* implementar `StateManager` HMAC single-use y validación estricta en el callback **antes** de que `OAuthDiscordCallback` deje de devolver 501.

### Fase 3 — Próximo sprint
1. **SEC-005** — Validar `RequireEncryptionKey` (+ decodificación a 32 bytes) al boot cuando M3 use cifrado.
2. **SEC-006** — Validar `len(ev.Nonce) == gcm.NonceSize()` en `Decrypt`.
3. **SEC-004** — Garantizar `request_id` presente en todo log de pánico.

### Fase 4 — Backlog / endurecimiento
1. **SEC-007** — Construir la URL OAuth con `net/url` (relevante al cambiar `state` a HMAC en M3).
2. **SEC-008** — `safeDSNPrefix`: parsear el DSN y registrar solo host/db, nunca userinfo.
3. **SEC-009** — Reemplazar el fallback `Store==nil ⇒ noopAuth` por un flag de test explícito.

---

## Cobertura del Audit

| Área | Archivos Analizados | Cobertura |
|------|---------------------|-----------|
| Layer A — middleware de autenticación | `middleware.go` (+ test) | Alta |
| Layer B — authz / decisiones | `authz.go` (+ test) | Alta |
| Handlers de roster (agents) | `agents.go` (+ test) | Alta |
| Store pgx (SQL / parametrización) | `postgres.go`, `store.go` | Alta |
| Secrets (AES-GCM + redacción) | `secrets.go`, `observability.go` | Alta |
| Worker proyección de rol | `project_agent_role.go` (+ test) | Alta |
| Config / arranque | `config.go`, `cmd/api/main.go`, `cmd/keygen/main.go` | Alta |
| Esquema (api_keys, users, oauth_tokens) | `migrations/0001_init.sql` | Alta |
| Router / CORS / wiring | `router.go` (+ test) | Alta |
| Discord client (MANAGE_ROLES) | `discord.go` | Alta |
| OAuth (state / callback) | `oauth.go`, `transversal.go` | Alta (seam — implementación es M3) |

## Limitaciones del Análisis
- Análisis **estático** del código M1. No se ejecutó la aplicación, ni se probó comportamiento en runtime, ni se hizo fuzzing del flujo OAuth (cuyo callback es 501 hasta M3).
- No se evaluó la **infraestructura de despliegue** (TLS-termination, secret manager real, configuración de red, `sslmode` efectivo de Postgres en producción — el `.env.example` lo documenta correctamente como `require`/`verify-full` fuera de local, pero no se verificó el despliegue real).
- No se auditó la **seguridad de dependencias / CVEs** de los módulos Go (`go.sum`): fuera del alcance de M1 (columna de authZ) según el encargo; recomendable un `govulncheck` en M5 (endurecimiento OSS).
- El cifrado de `oauth_tokens` y el rate-limit distribuido de Discord se evaluaron a nivel de **primitiva**; su uso real (escritura de tokens, llamadas a Discord) llega en M2/M3 y debe re-auditarse entonces.
- Las rutas M2+ (`/channels`, `/collaborators`, idempotencia, jobs) son stubs 501 y quedan fuera de esta auditoría de M1.

---

## Re-check (control-plane fix)
**Fecha:** 2026-06-08 · **Modo:** focused re-check (Iteración 1) · **Alcance:** la reimplementación del gate de roster como "autoridad de plano de control" + tres endurecimientos.

### Veredicto: **CONFIRMADO** — el arreglo cierra SEC-001 sin introducir un bypass (CWE-639 ni otro). **0 Critical, 0 High nuevos.**

El gate de roster pasó de *inalcanzable* (`Principal.IsAdmin` siempre `false`) a *alcanzable y correctamente anclado en estado de Postgres*. La autoridad se deriva de `api_keys.scope` — valor del lado servidor fijado en la creación de la clave, recuperado por hash de la clave bearer autenticada — y de ningún input del cliente. La trampa latente que advertí en SEC-001 (derivar admin de algo controlable por el llamante) **no se materializó**: la implementación escogió la Opción B (scope explícito del servidor) y la cableó de forma segura.

### Verificación punto por punto

| # | Foco | Resultado | Evidencia |
|---|------|-----------|-----------|
| **1** | **Fuente de autoridad server-side, no controlable por cliente (CWE-639)** | ✅ Confirmado | `Principal.KeyScope` se puebla **exclusivamente** desde `apiKey.Scope` (`middleware.go:120`), y `apiKey` proviene de `LookupActiveAPIKeyByHash` que hace `SELECT ... scope ... FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL` (`postgres.go:248-251`) — el `scope` sale de la fila de la DB indexada por el hash SHA-256 de la clave bearer. Grep exhaustivo de `KeyScope`/`c.Set(principal...)` en todo `**/*.go`: **el único escritor de producción de `KeyScope` es `middleware.go:120`**; los demás (`agents_test.go:177`, `authz_test.go:82,109`, `admin_gap_test.go`) son tests. **No existe ninguna ruta** por la que un header, body, query param u otro input del cliente pueble o altere `KeyScope`/`IsAdmin`. |
| **2** | **Exact-match, sin aflojar (constante, no prefijo/substring/case-insensitive/truthy)** | ✅ Confirmado | `RequireControlPlane` (`authz.go:76-86`) usa `p.KeyScope == ScopeBackoffice` — igualdad estricta contra la constante `ScopeBackoffice = "backoffice"` (`authz.go:64`), no `HasPrefix`/`Contains`/`EqualFold`. El único otro grant es `return p.IsAdmin` (camino futuro usuario/sesión, `authz.go:85`). Un scope más estrecho ⇒ `Deny` ⇒ 403, probado por `TestRosterAPI_NarrowScopedKey_Returns403` (`admin_gap_test.go:194-211`) y `TestAuth_NonBackofficeScope_DoesNotGrantControlPlane`, que cubre explícitamente `"admin"`, `"superuser"`, `"BACKOFFICE"`, `"Backoffice"`, `""` — **todos denegados**, confirmando que ni un scope semánticamente "admin" ni variantes de mayúsculas/minúsculas conceden autoridad. |
| **3** | **Gate de roster cableado; 401 fail-closed sin cambios** | ✅ Confirmado | Los tres handlers llaman `RequireControlPlane` como primer gate y devuelven `forbidden(c)` (403) si falla: `ListAgents` (`agents.go:23-27`), `AddAgent` (`agents.go:59-63`), `RemoveAgent` (`agents.go:136-140`). Las tres rutas están montadas bajo el grupo `v1` con `Auth` como middleware de grupo (`router.go:62,95-97`), así que Layer A rechaza no-autenticados con 401 **antes** de llegar al handler — fail-closed intacto, probado por `TestRosterAPI_Unauthenticated_Returns401` (`admin_gap_test.go:175-188`). El callback OAuth exento (`router.go:51`) se registra como ruta discreta fuera del grupo `v1`; por enrutado exacto de Gin no solapa con `/agents`, por lo que no abre un hueco de exención. |
| **4a** | **`safeDSNPrefix` ya no registra `user:pass`** | ✅ Confirmado | Reescrito (`postgres.go:383-417`): para DSN URL-style elimina todo antes de `@` (`rest[at+1:]`, línea 388-390) y descarta la query string; para key=value extrae **solo** `host` y `dbname`, saltando `password` y todo lo demás (línea 400-413); formato no reconocido ⇒ `"[dsn-format-unknown]"` (línea 416), nunca el DSN crudo. La heurística insegura de "primeros 40 chars" (origen de SEC-008) está eliminada. **SEC-008 resuelto.** |
| **4b** | **`ValidateEncryptionKey` rechaza clave ausente/longitud incorrecta al boot** | ✅ Confirmado | `ValidateEncryptionKey` (`config.go:129-141`): vacía ⇒ error; no-base64 ⇒ error; decodifica a ≠32 bytes ⇒ error. Invocada en el arranque de `cmd/api` con `os.Exit(1)` ante fallo (`main.go:42-45`), junto a `RequirePostgresDSN`/`RequireDiscordToken`. El patrón "fail loudly at startup" ahora **sí** cubre la clave de cifrado. **SEC-005 resuelto** (antes de tiempo respecto a M3). |
| **4c** | **`secrets.Decrypt` protege contra ciphertext corto (sin panic)** | ✅ Parcial — sin panic, pero la guarda sigue sobre el campo equivocado | `Decrypt` (`secrets.go:101-103`) mantiene `if len(ev.Ciphertext) < gcm.NonceSize() { return ErrCiphertextTooShort }`. Esto previene el caso de ciphertext degenerado, pero **el nonce se almacena separado del ciphertext** (`secrets.go:63,75`), así que la comprobación que faltaba — y sigue faltando — es `len(ev.Nonce) == gcm.NonceSize()`. Un `ev.Nonce` de longitud inválida (fila corrupta) aún haría `panic` en `gcm.Open` (`secrets.go:105`). **SEC-006 persiste** como Bajo: no explotable en M1 (no hay lectura/escritura de `oauth_tokens` hasta M3), recuperado por el recovery del API/worker, pero la guarda correcta debe añadirse antes de que M3 lea tokens. No es un hallazgo nuevo ni bloquea entrega. |

### Conclusión
- **SEC-001 cerrado.** La autoridad de plano de control es una función pura del estado de Postgres (`api_keys.scope`), inalcanzable desde input del cliente; el gate es exact-match contra una constante; las tres rutas lo aplican y el 401 de Layer A es fail-closed. **No se introdujo CWE-639 ni ningún otro bypass.** Reclasifico SEC-001 de Medio (disponibilidad/trampa latente) a **Resuelto**.
- **SEC-005 y SEC-008 resueltos** por los endurecimientos 4b y 4a.
- **SEC-006 persiste como Bajo** (guarda de nonce; el endurecimiento 4c evita el caso de ciphertext corto pero no valida `len(ev.Nonce)`). No bloquea M1; cerrar antes de que M3 lea `oauth_tokens`.
- **Sin hallazgos Critical/High nuevos.** Los Medium pendientes de la auditoría base que NO eran objeto de esta iteración siguen abiertos según su calendario: **SEC-002** (state OAuth HMAC — gate de M3) y **SEC-003** (cabeceras de seguridad + rate-limiting en el borde).

**Postura tras el arreglo:** la columna de authZ de M1 es segura para entrega. El plano de control quedó funcional y correctamente anclado, sin abrir superficie de escalación.
