# Observabilité d'une API Go : slog, runtime/metrics, Prometheus et OpenTelemetry

[English version](README.md)

Une petite API de factures en `net/http` standard, instrumentée pour que les
trois signaux renvoient les uns aux autres :

- **logs** (`log/slog`, JSON) : chaque ligne de log d'une requête porte `trace_id` et `span_id` ;
- **métriques** (Prometheus, méthode RED + runtime Go) : les requêtes lentes ou en
  erreur portent leur `trace_id` en **exemplar**, si bien qu'un point du graphe de
  latence dans Grafana ouvre la trace ;
- **traces** (OpenTelemetry) : span serveur par `otelhttp`, span enfant dans le
  dépôt, `traceparent` W3C entrant gardé comme lien (endpoint public),
  échantillonnage en tête dans l'app et **tail sampling** dans le Collector (erreurs et requêtes lentes gardées, ~10 % du reste).

Code d'accompagnement de l'article « Observabilité d'une API Go : slog, Prometheus et
OpenTelemetry reliés par un trace_id », par Odilon Hugonnot ([web-developpeur.com](https://web-developpeur.com)).

C'est un exemple pédagogique, pas un modèle de production.

## Prérequis

- **La pile** (`make up`, `make demo`, `make e2e`) : Docker Engine avec Compose v2
  (`docker compose version`) ; `curl` et `jq` ne servent qu'à `make e2e`. Go n'est pas nécessaire : l'API et
  le générateur de charge sont compilés dans des conteneurs.
- **Le développement local** (`make run`, `make test`, `make lint`, `make vuln`) :
  Go 1.26 (la ligne `toolchain` de `go.mod` demande go1.26.10).
- `make`, ou lancer à la main les commandes du `Makefile`.

## Organisation

```
main.go          config, serveurs (public :8080, métriques internes :9091), timeouts, arrêt sur SIGTERM
server.go        routes, validation des entrées, ordre des middlewares
store.go         dépôt en mémoire, span enfant, Invoice.LogValue (pas de données perso dans les logs)
logging.go       handler slog qui ajoute trace_id/span_id, filet de sécurité de masquage
metrics.go       métriques RED par motif de route, exemplars, récupération des panics, runtime Go
tracing.go       TracerProvider, ParentBased(TraceIDRatioBased), limites de span, exporteur OTLP
demo.go          ?delay= et ?fail= (seulement avec DEMO_FAULTS=1)
cmd/loadgen      générateur de trafic : requêtes normales, lentes et en erreur
*_test.go        httptest, tracetest, prometheus/testutil, corrélation logs/traces
docker-compose.yml, Dockerfile, otel-collector/, prometheus/, grafana/   le labo local
scripts/e2e.sh   test de bout en bout de toute la pile
```

## Lancer

```sh
make run          # DEMO_FAULTS=1 go run .   (API sur :8080, métriques sur 127.0.0.1:9091)
curl localhost:8080/invoices/1
curl -X POST localhost:8080/invoices -d '{"customer":"Bob","email":"bob@example.com","amount_cents":500,"currency":"EUR"}'
curl 127.0.0.1:9091/metrics
```

Sans `OTEL_EXPORTER_OTLP_ENDPOINT`, les spans sont créés (les logs ont toujours
un `trace_id`) mais pas exportés.

```sh
make test         # go test ./...
make lint         # go vet, gofmt -l, staticcheck
make vuln         # govulncheck
make rules        # promtool check rules / test rules / check config (Docker)
```

### La pile complète

```sh
cp .env.example .env    # puis renseigner GRAFANA_ADMIN_PASSWORD (voir plus bas)
make up                 # démarre la pile
make demo               # conteneur loadgen : 1000 requêtes, 5 % d'erreurs, 5 % de lentes
make down               # l'arrête et supprime ses volumes (marche sans .env)
make e2e                # son propre projet et ses propres ports : up, démo, vérifications, down -v
```

Un mot de passe : `openssl rand -base64 18`, ou sans openssl
`head -c 18 /dev/urandom | base64`. Avec un mot de passe vide,
`docker compose` refuse de démarrer : c'est voulu.

Sans `make` :

```sh
docker compose up -d --build --wait
docker compose --profile tools run --rm --build loadgen              # = make demo
docker compose --profile tools run --rm loadgen -n 3000 -errors 0.1  # autres mélanges
GRAFANA_ADMIN_PASSWORD=unused docker compose --profile tools down -v # = make down
./scripts/e2e.sh                                                     # = make e2e
```

Ports de l'hôte, tous liés à 127.0.0.1 :

| Service    | Pile de dev (`make up`), variable | `make e2e` par défaut, surcharge |
|------------|-----------------------------------|----------------------------------|
| API        | 8080, `APP_PORT`                  | 18080, `E2E_APP_PORT`            |
| Grafana    | 3000, `GRAFANA_PORT`              | 13000, `E2E_GRAFANA_PORT`        |
| Jaeger UI  | 16686, `JAEGER_PORT`              | 26686, `E2E_JAEGER_PORT`         |
| Prometheus | 9090, `PROMETHEUS_PORT`           | 19090, `E2E_PROMETHEUS_PORT`     |

Le port de métriques de l'app (9091) et le Collector ne sont jamais publiés. Le
test e2e a son propre projet compose et ses propres ports : il peut tourner
pendant qu'une pile de dev est lancée ; un seul e2e à la fois.

| URL                         | Quoi                                        |
|-----------------------------|---------------------------------------------|
| http://127.0.0.1:8080       | l'API                                       |
| http://127.0.0.1:3000       | Grafana (admin / mot de passe du .env), tableau « Invoice API - RED » |
| http://127.0.0.1:16686      | interface Jaeger                            |
| http://127.0.0.1:9090       | Prometheus (alertes sous /alerts)           |

```
app ──OTLP/HTTP──> otel-collector (tail_sampling) ──OTLP/gRPC──> jaeger
 │ :9091/metrics            │ :8888 métriques internes
 └──────────── prometheus (stockage des exemplars, règles SLO) <───┘
                         grafana : datasources Prometheus + Jaeger, exemplar -> trace
```

## Le parcours en 2 minutes

1. `make up`, puis `make demo`, puis attendre environ 15 secondes (le
   Collector décide au bout de 5 s, Prometheus scrape toutes les 5 s).
2. Ouvrir Grafana (http://127.0.0.1:3000), tableau **Invoice API - RED**.
3. Dans le panneau **Duration**, les losanges sont des exemplars : survoler un
   losange, cliquer sur le lien `trace_id`. La trace s'ouvre dans Jaeger : span
   serveur, enfant `store.Get`, `demo.delay` pour une requête lente.
4. Copier ce `trace_id` et retrouver les lignes de log correspondantes :
   `docker compose logs app | grep <trace_id>`.

Peu de losanges ? Prometheus garde environ un exemplar par bucket
d'histogramme et par scrape, et seules les erreurs et les requêtes lentes en
portent un. Relancez `make demo` deux ou trois fois.

## Ce que montre chaque brique

**Labels par motif de route.** Les métriques sont étiquetées avec `r.Pattern`
(`/invoices/{id}`), jamais `r.URL.Path` (`/invoices/42`) : une série par route,
pas une par facture. Une requête sans route reçoit `route="unmatched"`, une
méthode inconnue `method="OTHER"` : un client ne peut pas créer de séries à volonté.

**Ordre des middlewares.** `otelhttp -> instrument -> mux`. Le mux écrit
`r.Pattern` dans le `*http.Request` qu'il reçoit ; `instrument` doit lui passer
ce même pointeur (pas de `r.WithContext`) pour le relire ensuite. Copier la
requête casse d'un coup les labels, les noms de spans et les exemplars : six
tests échouent.

**Panics.** `instrument` récupère un handler qui panique, le logge (type et
pile) et le compte en `code="500"` : un crash ne peut pas échapper au taux
d'erreur. Si rien n'a encore été écrit, le client reçoit une 500 ; si une partie
de la réponse est déjà partie, la connexion est coupée (`http.ErrAbortHandler`)
pour que le client voie un échec plutôt qu'une 200 tronquée. Un handler qui
panique lui-même avec `http.ErrAbortHandler` abandonne volontairement : la
panique est relancée, sans être comptée.

**Port de métriques séparé.** `/metrics` écoute par défaut sur `127.0.0.1:9091`
(`:9091` dans le compose, où ce port n'est pas publié). Ce n'est pas l'API, cela
révèle l'interne (version de Go, mémoire, routes), et le scrape ne doit pas
dépendre de l'authentification, du rate limiting ou du load balancer du port public.

**Logs.** `traceHandler` enveloppe le handler JSON et ajoute `trace_id`/`span_id`
depuis le contexte (utiliser `InfoContext` et consorts). Les logs d'accès des
5xx sont au niveau `ERROR`. Comme tout attribut ajouté par un handler,
`trace_id` atterrit dans le groupe courant d'un logger créé avec `WithGroup` ;
le logger des requêtes n'est jamais groupé ici, il reste donc au premier niveau,
là où les outils de logs le cherchent. Les erreurs du SDK OpenTelemetry lui-même
(un export raté…) vont dans le même log JSON, au niveau `ERROR`
(`otel.SetErrorHandler`).

**Les sondes de santé sont invisibles.** `GET /healthz` reçoit sa réponse avant
toute instrumentation : ni span, ni métrique, ni log d'accès. Comptées, les
seules sondes du healthcheck du compose (toutes les 5 s) ajouteraient des
succès gratuits au dénominateur du taux d'erreur et masqueraient de vraies
erreurs, plus 17 000 lignes de log par jour.

**Endpoint public ou propagation interne.** Sur ce port public, un
`traceparent` entrant n'est pas fiable : avec `otelhttp.WithPublicEndpointFn`,
chaque requête démarre une nouvelle trace, échantillonnée selon notre propre
politique, et se contente d'un *lien* vers le contexte de l'appelant. Sans cela,
un client pourrait choisir notre `trace_id`, ou envoyer le flag « non
échantillonné » (`-00`) pour faire disparaître jusqu'à ses 500 des traces et des
exemplars. Entre services internes de confiance, on veut l'inverse : une
propagation simple, une seule trace à travers les services. Le compromis : une
trace démarrée par un front de confiance est coupée en deux ici, reliée par le lien.

**Données personnelles.** La défense, c'est de ne pas les logger :
`Invoice.LogValue` ne logge que l'id, le montant et la devise, le log d'un corps
invalide ne recopie pas le corps, les identifiants invalides sont rejetés avant
d'atteindre logs et spans. Un filet `ReplaceAttr` met chaque clé
d'attribut en minuscules et retire `_`, `-` et `.`, puis la masque si elle vaut
l'un des mots `email`, `password`, `passwd`, `token`, `secret`, `secretkey`,
`privatekey`, `accesskey`, `cookie`, `authorization`, `apikey`, ou se termine
par l'un d'eux : `user_email`, `accessToken`, `X-Api-Key`, `userpassword`,
`db.password` sont masqués, `token_count`, `max_tokens` ou `email_verified`
non. Les pluriels (`emails`, `cookies`) et d'autres noms (`credentials`,
`session_id`) ne sont pas couverts.
Ses limites : il ne voit
que les clés d'attributs, donc un secret dans une map, dans une struct sans
`LogValue` ou dans un message d'erreur passe quand même.

**Entrées bornées.** Les identifiants doivent respecter `^[0-9]{1,18}$`,
`customer` et `email` sont limités à 256 octets, la devise fait trois lettres
majuscules ASCII, le corps est limité à 1 Mio (413 au-delà), les données après
l'objet JSON sont refusées (400), les valeurs d'attributs de span sont tronquées
à 256 octets, et les deux serveurs ont des timeouts de lecture, d'écriture,
d'inactivité et d'en-têtes, plus une limite d'en-têtes de 16 Kio.

**Métriques runtime.** `collectors.NewGoCollector` avec
`WithGoCollectorRuntimeMetrics(MetricsGC, MetricsMemory, MetricsScheduler)`
expose des séries de `runtime/metrics` (GC, tas, latences de l'ordonnanceur).

**Échantillonnage.** L'app échantillonne en tête avec
`ParentBased(TraceIDRatioBased(TRACE_SAMPLE_RATIO))` (1 par défaut). Sur cet
endpoint public chaque requête est une racine, donc c'est le ratio qui décide ;
`ParentBased` compte pour les spans enfants et pour les services internes qui
font confiance à leurs appelants. Dans le compose le
ratio vaut 1 et le Collector décide en queue, une fois la trace complète
(`decision_wait: 5s` pour la démo) : `status_code: ERROR` et `latency > 500ms`
sont toujours gardées, `probabilistic: 10` garde une part du reste. Ne combinez
pas un ratio de tête inférieur à 1 avec le tail sampling si vous voulez toutes
les erreurs : la tête abandonne des traces avant que le Collector sache qu'elles
ont échoué.

**Exemplars.** L'histogramme de durée utilise `ObserveWithExemplar` avec le
`trace_id`, mais **seulement pour les requêtes que le Collector garde à coup
sûr** : une 5xx, ou une requête d'au moins 500 ms (`slowThreshold`, la même
valeur que la politique de latence du Collector). Une 200 rapide n'est gardée
qu'une fois sur dix : son exemplar mènerait le plus souvent à une trace
absente. `/metrics` sert l'OpenMetrics (nécessaire aux exemplars) et Prometheus
tourne avec `--enable-feature=exemplar-storage`. La datasource Prometheus de
Grafana relie `trace_id` à la datasource Jaeger.

**Alertes sur le budget d'erreur.** `prometheus/rules/slo.yml` : SLO de 99,9 %
sur 30 jours, burn rate multi-fenêtres. Page à 14,4× (2 % du budget en 1 h,
vérifié sur 1 h et 5 min), ticket à 6× (5 % en 6 h, sur 6 h et 30 min). Seules
les 5xx comptent : une 404 est une erreur du client. `slo_test.yml` contient
huit cas `promtool test rules` : brûlage soutenu (pending, puis firing),
incident récent après une période calme, pic terminé avec des 404 continues,
erreurs très récentes seules, brûlage lent (pending, puis firing), brûlage lent
terminé, erreurs dans le budget, aucune erreur (ratio 0 et non « no data » :
les règles utilisent `(erreurs or total * 0) / total`).

### Les logs dans Loki (non inclus)

La pile n'a pas de stockage de logs : ils partent sur stdout. Pour passer d'une
ligne de log à sa trace, envoyer les lignes JSON à Loki (Grafana Alloy, ou le
receiver `filelog` du Collector qui lit les logs des conteneurs) et ajouter un
champ dérivé à la datasource Loki :

```yaml
jsonData:
  derivedFields:
    - name: trace_id
      matcherRegex: '"trace_id":"(\w+)"'
      datasourceUid: jaeger
      url: "$${__value.raw}"
```

Laissé de côté pour garder un labo petit : il faut accéder aux fichiers de logs
de Docker ou à un plugin de logging driver, donc plus de privilèges que le reste.

## Choix

- **Images séparées plutôt que `grafana/otel-lgtm`.** Cette image est parfaite
  pour un premier coup d'œil, mais le tail sampling exige le Collector *contrib*
  (`tail_sampling` n'est pas dans la distribution core), et voir la configuration
  de chaque composant est l'objet de l'article.
- **Jaeger 2.20.0, pas 2.22.** La
  [note de version v2.21.0](https://github.com/jaegertracing/jaeger/releases/tag/v2.21.0)
  annonce, dans les changements cassants, « remove v1 http endpoints the ui no
  longer calls ([#9260](https://github.com/jaegertracing/jaeger/pull/9260)) ».
  La datasource Jaeger de Grafana 13.2 les appelle encore. Reproduit le
  28/10/2026 avec jaegertracing/jaeger:2.22.0 et grafana/grafana:13.2.5 :
  `GET /api/services` sur Jaeger renvoyait 404, et
  `GET /api/datasources/uid/jaeger/health` sur Grafana renvoyait
  `{"message":"request failed: 404 Not Found","status":"ERROR"}`. Passer à Tempo
  est la vraie solution à terme.
- **Durcissement, là où beaucoup de tutoriels font autrement :** aucun conteneur
  en root, `cap_drop: ALL`, `no-new-privileges`, systèmes de fichiers en
  lecture seule, limites de mémoire et de processus (avec `GOMEMLIMIT` à ~90 % de celle de
  l'app), healthchecks (l'app se
  sonde elle-même avec `-healthcheck` : son image distroless n'a pas de shell),
  mot de passe Grafana dans `.env` (jamais `admin/admin` dans le
  compose), interfaces d'administration sur `127.0.0.1`, Prometheus sans
  `--web.enable-lifecycle`, images Go distroless `nonroot`.

## Versions

Go 1.26 (toolchain go1.26.10), go.opentelemetry.io/otel v1.47.0, otelhttp v0.72.0,
prometheus/client_golang v1.25.0, golang.org/x/net v0.62.0. Images :
otel/opentelemetry-collector-contrib:0.164.0, jaegertracing/jaeger:2.20.0,
prom/prometheus:v3.15.1, grafana/grafana:13.2.5, golang:1.26.10-alpine3.24,
gcr.io/distroless/static-debian13:nonroot (épinglée par digest).

## Vérification

Lancé par l'auteur le 28/10/2026 (WSL2, Docker 29.1.3, Compose v2.40.3).

| Quoi | Comment, et résultat |
|------|----------------------|
| format, vet, analyse statique | `gofmt -l .`, `go vet ./...`, `staticcheck ./...` (v0.8.2) : propre |
| tests unitaires, détecteur de courses | `go test -race ./...` dans `golang:1.26.10-bookworm` : vert |
| vulnérabilités connues | `govulncheck ./...` (v1.9.0) : aucune |
| règles d'alerte | `promtool check rules`, `promtool test rules` (8 cas), `promtool check config`, dans `prom/prometheus:v3.15.1` |
| verdict du générateur de charge | `go test ./cmd/loadgen` : petit n, aucune requête en échec, 5xx manquants ou en trop, flags NaN |
| config du Collector | `otelcol-contrib validate` dans l'image 0.164.0 |
| pile complète | `scripts/e2e.sh` (la cible `make e2e`) vert deux fois de suite depuis un clone neuf, avec un PATH sans Go, pendant qu'une pile de dev (`docker compose up -d`, ports par défaut) tournait depuis un autre clone : 1000 requêtes comptées, labels de route limités aux motifs, au moins 80 % des 5xx et des requêtes lentes retrouvées dans Jaeger, 70 à 90 % des traces abandonnées (observé : 79-83 %), chaque exemplar retrouvé dans Jaeger et dans les logs de l'app, règles d'alerte chargées, datasources Grafana en bonne santé, `/metrics` en 404 sur le port public |
| les tests attrapent les régressions | 54 mutations Go et 18 mutations de règles, chacune a fait rougir un test (script de mutation local, non publié) : par exemple `r.URL.Path` en label -> `TestRequestCounterUsesRoutePattern`, pas de `recover` -> `TestPanicIsCountedAs500`, pas d'abandon après une écriture partielle -> `TestPanicAfterWriteAbortsConnection`, pas d'endpoint public -> `TestUnsampledTraceparentCannotHideA500`, sondes de santé comptées -> `TestHealthzIsNotCounted`, `validID` jusqu'à 40 chiffres -> `TestIDLengthBoundary`, exemplar sur toutes les requêtes -> `TestExemplarsOnlyForKeptTraces`, `for: 2m` -> `40m` -> le cas pending/firing de `slo_test.yml`. Retirer la politique `errors` du Collector fait échouer l'e2e (« error traces in Jaeger: 4, want >= 80% of 45 ») |
| incompatibilité Jaeger 2.22 | reproduite, voir Choix |

Non vérifié : la tenue en charge réelle ou avec plusieurs réplicas du Collector,
le clic exemplar → trace
dans l'interface Grafana elle-même (vérifié par les API de Grafana et de
Prometheus), les hôtes arm64, les cibles `make` sur la machine de l'auteur
(`make` n'y est pas installé : les mêmes commandes ont été lancées directement,
et `make -n` / `make lint test` dans l'image Go).

## Limites

- Exemple pédagogique : dépôt en mémoire (non borné : chaque POST y reste
  jusqu'au redémarrage), pas d'authentification, pas de TLS
  entre composants, Jaeger en mémoire, intervalles de démo (scrape 5 s,
  `decision_wait` 5 s).
- `DEMO_FAULTS` doit valoir `0` hors démo (`DEMO_FAULTS=0 make up`).
- Le `trace_id` du log d'une requête rapide et réussie peut viser une trace que
  le Collector a abandonnée (environ 90 % du trafic normal).
- Le tail sampling exige que tous les spans d'une trace passent par le même
  Collector : avec plusieurs réplicas, placer un exporteur `loadbalancing` devant.

## Licence

MIT, Copyright (c) 2026 Odilon Hugonnot.
