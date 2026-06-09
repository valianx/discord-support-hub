# Servicio de Espacios de Soporte en Discord

> Documento de requerimientos y diseño (nivel funcional / arquitectónico).
> El detalle técnico fino (contratos OpenAPI, esquema de datos, código) se define después.
> Nombre del proyecto: **por decidir** (ver §11). Nombre de trabajo: `discord-support-hub`.
> Fuente: [Google Doc original](https://docs.google.com/document/d/1ZTt9v8gaIYGbHiWWwdXTIAbI00ttfm0_w_jlkQBFGc4/edit)

---

## 1. Resumen

Servicio **open-source** y **API-driven** que aprovisiona y gobierna **espacios de soporte privados y aislados sobre Discord**, uno por cliente externo (merchant), con control de acceso por rol. Está pensado para que un equipo interno (agentes) atienda a colaboradores externos en canales que son **invisibles por defecto** y a los que solo se accede por invitación gestionada por API.

Filosofía de diseño: **mechanism, not policy**. El sistema provee los mecanismos (aprovisionamiento, ACL, marcado, visibilidad del help desk) con defaults sanos y degradación elegante; la lógica de negocio y la configuración quedan en manos de quien lo despliega.

---

## 2. Contexto y decisión de plataforma

El objetivo es dar soporte a merchants con dos características clave:

- **Aislamiento real entre clientes:** un colaborador solo puede ver los espacios a los que fue invitado; jamás los de otros merchants.
- **Visibilidad total para el equipo:** los agentes ven todos los espacios.

Se evaluaron varias alternativas (helpdesks tipo Freshdesk/Chatwoot, Slack Connect, Matrix/Rocket.Chat/Mattermost, bots de Telegram con topics). Telegram quedó descartado porque **no tiene permisos por topic** (todos los miembros de un supergrupo ven todos los topics), lo que rompe el aislamiento de clientes. Discord se eligió porque ofrece **ACL real por canal** vía *permission overwrites*, que es exactamente el modelo requerido.

**Trade-off aceptado:** Discord es SaaS, no self-hosted. Los datos de conversación viven en Discord, no en infraestructura propia. El servicio de aprovisionamiento sí es propio y open-source.

---

## 3. Conceptos y glosario

- **Agente:** miembro del equipo interno (Zippy). Lectura/escritura en *todos* los espacios; puede invitar, expulsar y listar. Es un humano con su propia cuenta de Discord.
- **Admin:** un Agente con privilegio adicional para gestionar el roster de Agentes (alta/baja). No es un tercer rol de cara al cliente, es una salvaguarda.
- **Colaborador:** invitado externo de un merchant. Acceso solo a los espacios a los que fue invitado; no puede invitar ni expulsar.
- **Espacio:** la conversación privada por merchant. Puede materializarse como **canal** o como **thread privado** (ver §4.4).
- **Merchant:** el cliente al que pertenece un conjunto de colaboradores y su(s) espacio(s).
- **Bot administrativo (silencioso):** la aplicación con bot token que ejecuta las acciones por API (crear canales, aplicar overwrites, agregar miembros, marcar agentes). **Nunca conversa ni aparece como agente**; solo administra. Es obligatorio para cualquier operación por API.

---

## 4. Modelo de acceso y roles

### 4.1 Dos roles + capa Admin

- **Agente** y **Colaborador** son los dos roles del modelo (nombres configurables).
- **Admin** es un Agente con privilegio para gestionar el roster.

### 4.2 Fuente de verdad y proyección

- La verdad de "quién es Agente/Colaborador/Admin" vive en la **base de datos propia** (Postgres).
- El rol de Discord es la **proyección** de ese registro: el bot lo asigna y lo mantiene en sincronía (reconciliación).
- **La autorización siempre se resuelve contra la base de datos, nunca confiando solo en el rol de Discord.**

### 4.3 ACL de los espacios

- Todo espacio nace **invisible**: deny `@everyone` → `VIEW_CHANNEL`.
- El rol **Agente** lleva allow `VIEW_CHANNEL` a nivel **categoría** → los agentes ven todos los espacios con un solo overwrite por categoría.
- Los **Colaboradores** acceden por **permission overwrite por usuario** sobre su espacio.
- **No se usan roles por merchant.** Discord limita a 250 roles por servidor; un rol por merchant choca con ese techo y con los rate limits. La agrupación por merchant vive en la base de datos, no en roles de Discord.

### 4.4 Canal vs thread (escala)

- Discord limita a 500 canales por servidor (50 por categoría).
- Para pocos/decenas de merchants: **canal por merchant** (más simple y permanente).
- Para muchos merchants: **thread privado por merchant** (no consume el presupuesto de 500 canales; escala a miles). Los threads privados solo los ven los miembros agregados + quien tenga Manage Threads (los agentes).

### 4.5 Marcado visual del Agente

- **Default gratis:** emoji como prefijo en el apodo del servidor (ej. `🛡️ Mario`), aplicado por el bot, + color distintivo + *hoist* (mostrar el rol aparte en la lista de miembros). Sin costo.
- **Upgrade opcional:** *role icon* (logo de Zippy) sobre el rol Agente, vía API. Requiere **Boost nivel 2**; si el servidor pierde el nivel 2, el icono deja de mostrarse.
- El sistema soporta ambos con degradación elegante: si hay Boost L2 usa el role icon; si no, cae a emoji + color + hoist.
- Nota: si un usuario tiene varios roles con icono, Discord muestra el del rol más alto.

---

## 5. Onboarding y acceso (solo por API, sin invites de Discord)

Dos capas distintas:

- **Acceso a canales:** siempre es *permission overwrite* (no existe "invite" a nivel canal). Ya es 100% por API.
- **Entrada al servidor:** se hace vía **OAuth2 con scope `guilds.join`** — el colaborador hace un "Connect with Discord" una sola vez, el backend guarda su access token y el servicio lo agrega al guild por API. **Sin invite links.**

Para que el bot pueda usar el endpoint *Add Guild Member* necesita el permiso `CREATE_INSTANT_INVITE`. Se resuelve dejando ese permiso **solo en el bot**: deny para `@everyone`, Agentes y Colaboradores. Así ningún humano puede generar invites, pero el bot sí puede aprovisionar.

---

## 6. Requerimientos funcionales

| ID | Requerimiento |
| :-- | :-- |
| FR-1 | Aprovisionar un espacio privado por merchant vía API (naming y categoría configurables). |
| FR-2 | Soportar modo **canal** y modo **thread privado**, seleccionable por configuración. |
| FR-3 | Aplicar ACL por espacio: deny `@everyone`, allow rol Agente (categoría), allow por usuario para Colaboradores. N colaboradores por espacio. |
| FR-4 | Gestionar membresía de colaboradores: agregar/quitar usuarios de un espacio. |
| FR-5 | Garantizar invisibilidad por defecto: un espacio solo es accesible tras una invitación ejecutada por un Agente; no se puede descubrir sin invitación. |
| FR-6 | Acceso de Agentes de lectura/escritura a todos los espacios (vía rol a nivel categoría). |
| FR-7 | Ciclo de vida de espacios: activo → resuelto → archivado, con reabrir; bloquear/ocultar sin borrar historial. |
| FR-8 | (Opcional) Espejar mensajes a un store externo para durabilidad, búsqueda y auditoría. *Módulo opcional, fuera del MVP.* |
| FR-9 | Mapeo de identidad como fuente única de verdad: merchant ↔ usuarios ↔ espacios. |
| FR-10 | Listar todos los espacios con estado, dueño, fecha de creación y última actividad. |
| FR-11 | Superficie de control (API admin y/o comandos) para aprovisionar, invitar, expulsar, listar, cerrar, reabrir. |
| FR-12 | (Opcional) Notificación/ruteo a agentes de nuevos mensajes; auto-asignación configurable. |
| FR-13 | Configuración declarativa (guild ID, rol agente, naming, modo, política de archivado) con defaults sanos. |
| FR-14 | Audit log de acciones de aprovisionamiento, membresía y ciclo de vida (quién, qué, cuándo). |
| FR-15 | **Visibilidad del help desk:** mensaje configurable presente vía (a) topic/pin estático al aprovisionar, (b) *sticky message* al fondo re-posteado por actividad, (c) disparo al unirse el usuario (canal o DM). Soporta link parametrizado por cliente. Sin broadcast por reloj. |
| FR-16 | Modelo de dos roles (Agente / Colaborador), nombres configurables, con capa Admin. |
| FR-17 | Control de usuarios por espacio: listar miembros de un espacio con su rol y merchant asociado. |
| FR-18 | Directorio global: todos los espacios × usuarios × rol, con búsqueda bidireccional (quién está en este espacio / en qué espacios está este usuario). |
| FR-19 | Expulsión por un Agente, con alcance configurable: remove-from-channel (revoca overwrite) o remove-from-server (saca del guild). Acción auditada. |
| FR-20 | Invitación restringida: solo Agentes pueden invitar/dar acceso; los Colaboradores no pueden invitar a nadie. |
| FR-21 | Endpoint "canales por colaborador": retorna los espacios a los que un colaborador tiene acceso (con merchant, rol y estado). |
| FR-22 | Provisioning solo por API: alta al servidor vía OAuth2 `guilds.join`; acceso por overwrites; sin invite links. |
| FR-23 | Gestión y marcado de Agentes: alta/baja por la capa Admin; `type` (agent/collaborator) e `is_admin` en el store como fuente de verdad; el bot proyecta y reconcilia el rol Agente. |
| FR-24 | Marcado visual del Agente configurable (role icon, color, hoist y/o prefijo de apodo) con degradación elegante según capacidades del servidor (p. ej. ausencia de Boost L2). |

---

## 7. Requerimientos no funcionales

| ID | Requerimiento |
| :-- | :-- |
| NFR-1 | **Escalabilidad:** diseñar alrededor de los límites de Discord (500 canales/guild, 50/categoría, 250 roles/guild). Soportar modo thread y/o sharding multi-guild. Definir capacidad objetivo. |
| NFR-2 | **Resiliencia ante la API de Discord:** respetar rate limits (global + por ruta) con cola, backoff y reintentos. |
| NFR-3 | **Idempotencia y reconciliación:** operaciones idempotentes (reintentar no duplica); reconciliación estado deseado (DB) vs real (Discord) con auto-reparación de drift. |
| NFR-4 | **Fail-closed:** si falla aplicar la ACL, el espacio queda sin acceso por defecto, jamás world-readable. |
| NFR-5 | **Aislamiento multi-tenant:** la separación entre clientes es un invariante de seguridad, verificable y testeable. |
| NFR-6 | **Manejo de secretos:** bot token y access tokens OAuth2 de colaboradores cifrados; redacción de secretos en logs. |
| NFR-7 | **Observabilidad:** logging estructurado, métricas (latencia de aprovisionamiento, espacios activos, rate-limit hits, errores), tracing OpenTelemetry (W3C), health checks. |
| NFR-8 | **Extensibilidad:** storage backend pluggable, estrategia de aprovisionamiento configurable, hooks/webhooks de eventos; lógica de negocio en userland. |
| NFR-9 | **Estado y recuperación:** store persistente del mapeo merchant↔espacio↔usuarios para sobrevivir reinicios; backups; sin pérdida de mapeo. |
| NFR-10 | **Portabilidad:** binario único (Go), imagen Docker, deps mínimas, config por env/archivo. |
| NFR-11 | **Rendimiento:** objetivo de latencia para dejar listo un espacio nuevo; uso de recursos acotado. |
| NFR-12 | **Retención/compliance:** política de retención del audit trail; borrado de datos de un cliente al darlo de baja. |
| NFR-13 | **AuthZ en dos capas:** las decisiones de autorización se resuelven contra el store; `MANAGE_ROLES` reservado al bot para que el rol Agente no sea auto-asignable; defensa en profundidad. |
| NFR-14 | **No-invites como invariante:** ningún humano (ni Agente) puede crear invites de Discord; `CREATE_INSTANT_INVITE` reservado al bot; todo acceso pasa por el servicio y queda auditado. |
| NFR-15 | **Anti-ruido / throttling:** el sistema nunca repite mensajes por reloj ciego; toda re-emisión (sticky, nudges) está gobernada por actividad + intervalo mínimo + tope diario, y es idempotente (editar la copia existente, no duplicar). |
| NFR-16 | **Calidad OSS:** documentación y ejemplos, tests (incluyendo integración contra un guild de prueba), semver, changelog, licencia definida (MIT/Apache). |

---

## 8. Superficie de API (alto nivel)

Contratos detallados (request/response, códigos) se definen después.

**Espacios**

- `POST /merchants/{merchantId}/channels` — aprovisionar (encola job)
- `GET /channels` — listar todos
- `GET /channels/{id}` — detalle
- `GET /channels/{id}/members` — usuarios del espacio + rol (FR-17)
- `POST /channels/{id}/lifecycle` — open/resolve/archive/reopen (FR-7)
- `POST /channels/{id}/welcome:sync` — mensaje/sticky de help desk (FR-15)

**Colaboradores**

- `POST /channels/{id}/collaborators` — invitar (overwrite + OAuth2 join si aplica)
- `DELETE /channels/{id}/collaborators/{userId}?scope=channel|server` — expulsar (FR-19)
- `GET /collaborators/{userId}/channels` — canales del colaborador (FR-21)

**Agentes (solo Admin)**

- `POST /agents` · `DELETE /agents/{userId}` · `GET /agents` (FR-23)

**Transversales**

- `GET /directory` — espacios × usuarios × rol (FR-18)
- `GET /audit` — log de acciones (FR-14)
- `GET /oauth/discord/callback` — "Connect with Discord" del colaborador (FR-22)

Las operaciones mutantes encolan jobs; los GET leen de cache. Toda la superficie resuelve AuthZ contra el store.

---

## 9. Help desk siempre disponible (detalle de FR-15)

El objetivo es que el link al help desk esté **siempre disponible** sin caer en spam. Se logra combinando tres mecanismos, **nunca un broadcast por reloj**:

1. **Presencia estática:** link en el topic/descripción del canal + mensaje fijado (pin). Siempre visible, cero ruido.
2. **Sticky message:** mensaje al fondo del canal que el bot re-postea **solo cuando hay actividad** que lo empuja hacia arriba (con intervalo mínimo entre re-posts para respetar rate limits). Una sola copia visible.
3. **Disparo al entrar:** al agregar al colaborador, postear o DM con el link una vez (el DM tiene mayor tasa de apertura para contenido importante).

Opcional: re-mostrar el link cuando el cliente escribe tras un periodo de inactividad, con tope (máximo una vez al día). Debounce por actividad, no por reloj.

---

## 10. Arquitectura y stack (POC)

**Stack:** Go + Gin (API) · asynq sobre **Valkey** (cola + workers) · discordgo (cliente Discord) · PostgreSQL (fuente de verdad) · OpenTelemetry.

**Rol de Redis/Valkey** (cache + coordinación, **nunca fuente de verdad**):

- **Cola async:** el endpoint de provisioning encola un job y responde rápido; un worker ejecuta las llamadas a Discord respetando el rate limit. Resuelve la concurrencia real.
- **Rate limiter distribuido / token bucket:** evita exceder los límites de Discord entre varios workers.
- **Idempotency keys:** evitan doble aprovisionamiento en reintentos (NFR-3).
- **Locks por espacio/merchant:** evitan races de overwrites recién creados.
- **Cache de lecturas:** directorio y listados con TTL + invalidación al escribir.

**Postgres = fuente de verdad** (roster, mapeos, AuthZ). La autorización jamás se resuelve contra Redis.

**Valkey vs Redis:** wire-compatibles (mismo protocolo RESP, mismo cliente go-redis sin cambios). Diferencia de licencia: Redis 8 es AGPLv3 (copyleft); Valkey es BSD-3-Clause (Linux Foundation). Recomendación para stack de empresa: **Valkey**, por cero ambigüedad de licencia; el docker image se cambia y el código es idéntico.

**Cuello de botella clave:** los rate limits de Discord (global + por ruta). La separación API-síncrona / worker-asíncrono es lo que hace robusto su manejo, y es una buena demo de arquitectura.

---

## 11. Nombre del proyecto (candidatos)

Tema de custodia / control de acceso a espacios privados:

- **Bastion** — punto único y controlado de acceso; resuena con "bastion host". Profesional, encaja con B2B/pagos. *(Recomendado.)*
- **Chamberlain** — el chambelán controlaba el acceso a las cámaras privadas. Distintivo y on-theme.
- **Postern** — puerta pequeña y privada de acceso restringido. Oscuro pero evocador.
- **Castellan** — guardián del castillo que controla el ingreso.
- **Concierge** / **Usher** — ángulo "anfitrión que acomoda invitados a su sección".

Revisar disponibilidad en npm/GitHub/dominio antes de fijar.

---

## 12. Decisiones abiertas / para después

> Resueltas en [`01-mvp-scope.md`](./01-mvp-scope.md) §4. Resumen del estado original:

- **Alcance del MVP:** qué FR entran en v1 (sugerido: FR-1..7, 9, 16..24; dejar FR-8 espejo de mensajes y FR-12 ruteo para v2).
- **Persistencia de mensajes (FR-8):** módulo opcional; define el peso de la DB. Arrancar solo gestionando accesos hace el MVP mucho más chico.
- **Identidad de Agente (v2):** atar "Agente" a identidad verificada de empresa (SSO/Workspace) en vez de allowlist manual.
- **Cascada de expulsión:** comportamiento por defecto de remove-from-server vs remove-from-channel.
- **Default del marcado visual:** emoji y color por defecto para Agentes.
- **Capacidad objetivo (NFR-1):** número de merchants/espacios esperado, que decide canal vs thread vs sharding.
- **Contratos de API y esquema de datos:** detalle técnico a definir (OpenAPI + DDL).
