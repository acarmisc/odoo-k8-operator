# Audit – odoo-k8-operator (16/10/2026)

Obiettivo: valutare l'operatore per un uso **in produzione con più istanze Odoo e moduli custom**.
Analisi sul codice a `45583eb`; le righe fanno riferimento a quella revisione.

## 1. Aggiornamento librerie (fatto in questo branch)

| Componente | Prima | Dopo |
|---|---|---|
| Go | 1.24 | **1.26** (richiesto da controller-runtime 0.25) |
| controller-runtime | v0.21.0 | **v0.25.2** |
| k8s.io/api, apimachinery, client-go | v0.33.0 | **v0.37.1** |
| ginkgo / gomega | v2.22.0 / v1.36.1 | v2.33.1 / v1.44.0 |
| controller-gen | v0.18.0 | v0.23.0 |
| kustomize | v5.6.0 | v5.8.3 |
| golangci-lint | v2.1.0 | v2.15.0 |
| envtest K8s | 1.33 | 1.37 |

Modifiche al codice rese necessarie dall'upgrade:
- `api/v1/odooaddon_webhook.go`: `NewWebhookManagedBy(mgr, &OdooAddon{})` e validator **tipizzato** `admission.Validator[*OdooAddon]` (niente più type-assert).
- CRD e deepcopy rigenerati. **Il CRD committato era obsoleto: mancava `spec.moduleName`**, quindi `make deploy` con il CRD del repo faceva scartare il campo all'API server (pruning).
- `Dockerfile` builder → `golang:1.26`; `build.yml` usa `go-version-file`.

Verifica: `go build`, `go vet`, `go test` (envtest K8s 1.37) verdi. Non testato: lint con v2.15.0, e2e su kind.

Non aggiornati/da verificare: `alpine:3.20` (EOL), `alpine/git:2.45.2`, `operator-sdk v1.42.0`, versioni delle GitHub Actions (checkout/setup-go/build-push) – vedi §4.

## 2. Bug e rischi trovati (ordinati per gravità)

### Alta – dati e correttezza
1. **Cancellare un `OdooInstance` cancella filestore e addons.** I PVC `<name>-data` e `<name>-addons` hanno `ownerReference` sull'istanza (`reconcileSinglePVC`): il GC elimina i dati. In produzione serve una retention policy esplicita (no ownerRef sul PVC dati, o campo `spec.dataVolume.retainOnDelete`, default sicuro = retain). Il finalizer dell'istanza oggi non fa nulla (`handleFinalizer`) e potrebbe ospitare questa logica.
2. **Cambiare `spec.config` non riavvia Odoo.** Il ConfigMap viene aggiornato ma il pod template non contiene un hash (`checksum/config`), quindi i pod restano con la configurazione vecchia.
3. **Sync dei moduli non atomica e senza riavvio.** `rm -rf` + `cp -a` direttamente sul PVC letto da Odoo: un pod può leggere un modulo a metà (il commento in `buildSyncScript` dichiara il contrario, vale solo per il clone). Inoltre Python non ricarica i moduli: dopo un nuovo commit nessun rollout e nessun `-u <modulo>` → codice su disco e in memoria divergono.
4. **Nessuna gestione di inizializzazione/upgrade DB** (`-i base`, `-u modulo`, migrazioni). Con moduli custom è il punto critico di ogni rilascio.
5. **Repository privati non supportati**: nessun riferimento a Secret per token/chiave SSH/known_hosts. Per moduli custom è il caso normale; l'unica via oggi è mettere credenziali nell'URL (finiscono nella spec, nei log e nelle env del Job).
6. **`replicas > 1` non funziona su più nodi**: PVC addons e data sono `ReadWriteOnce`, il Deployment fino a 100 repliche. I pod su nodi diversi restano `ContainerCreating`. Il Job di sync è fissato con `nodeSelector` al nodo del primo pod Running (workaround fragile: se il nodo cambia o è saturo il Job resta Pending).
7. **Default Odoo non sicuri/inadatti a produzione**: `admin_passwd` e `dbfilter` sono vietati in `spec.config` ma non c'è alternativa via Secret → master password Odoo di default e database manager esposto (`list_db` non disabilitato). Mancano `proxy_mode`, `workers`, `max_cron_threads`, `list_db=False`.

### Media – affidabilità operativa
8. **Reconcile bloccante**: `runSyncJob` fa `time.Sleep`/polling fino a 3 min dentro `Reconcile`; con `MaxConcurrentReconciles=1` (default) N addon vengono sincronizzati in serie e bloccano anche gli altri reconcile del controller. Va trasformato in macchina a stati non bloccante (crea Job → `Owns(Job)` → leggi status → requeue).
9. **Job di sync a ogni intervallo (5 min) anche se il ref non è cambiato**, sempre con delete+recreate. Usare `git ls-remote` (o confronto commit) e sincronizzare solo se cambia lo SHA.
10. **Il ref viene risolto solo come branch/tag** (`--branch`): commit SHA non supportati, nonostante il README. Per produzione è preferibile pinnare allo SHA (riproducibilità, rollback).
11. **Moduli rimossi non vengono ripuliti**: cancellare un `OdooAddon` o un modulo dal repo lascia i file nel PVC (Odoo continua a vederli). Due addon con lo stesso `moduleName` si sovrascrivono senza errore.
12. **Nessun probe** (startup/readiness/liveness) sul container Odoo, nessun `PodDisruptionBudget`, strategy di rollout, topology spread/anti-affinity, `terminationGracePeriodSeconds`. Con più repliche i rollout causano downtime.
13. **Solo porta 8069 esposta**: manca 8072 (websocket/longpolling per bus, chat, notifiche) – con `workers > 0` va instradata separatamente. Nessun Ingress/Gateway API HTTPRoute generato.
14. **Un unico Deployment per HTTP e cron**: con più repliche i cron girano su tutti i pod. Pattern consolidato: Deployment web + Deployment cron/worker dedicato (`max_cron_threads`, `--no-http`).
15. **PVC**: storage class di default hardcoded `"standard"` (non portabile: lasciare `nil` per usare la default del cluster); nessun resize (l'espansione è consentita da K8s ma il codice ignora le modifiche a `size`); PVC addons non necessario se si adotta il pattern "addons nell'immagine" (§3).
16. **Job di sync non hardened**: gira come root (`alpine/git`), senza `securityContext`, requests/limits, `activeDeadlineSeconds`; immagine non pinnata a digest e non configurabile.
17. **Secret**: `PGPASSWORD` e `DB_PASSWORD` duplicati, chiave fissa `password`; nessun supporto a rotazione (nessun rollout al cambio del Secret) né a Secret generati da CloudNativePG (`<cluster>-app`).
18. **`db_sslmode = require`** hardcoded e solo se `postgres.database` è valorizzato; nessuna CA custom.

### Bassa – qualità del codice
19. **Campi dichiarati ma ignorati**: `spec.version`, `spec.edition`, `OdooAddon.spec.readOnly`. Aspettative errate per chi usa l'API.
20. **`Deployment.Spec.Template` riassegnato per intero** a ogni `CreateOrUpdate`: si perdono i default dell'API server e ogni reconcile fa una scrittura inutile. Meglio mutare solo i campi di proprietà o usare **Server-Side Apply** (`client.Apply` con field manager).
21. **Status povero**: solo `phase`/`ready` booleani; niente `conditions` (`Available`, `Progressing`, `Degraded`, `AddonsSynced`) né `observedGeneration` per condizione; `Status().Update` sull'oggetto intero è soggetto a conflitti (usare patch).
22. **Nessun evento Kubernetes** (`EventRecorder`) né metriche custom (durata/esito sync, drift commit).
23. **Validazione duplicata**: regex in `odooaddon_webhook.go` e `odooaddon_controller.go`; il webhook è disattivato di default (`--enable-webhooks=false`) e i manifest webhook non sono collegati al kustomize. Vedi §3.1.
24. `validPathSegment` vieta `/`: impossibile puntare a una sottocartella annidata (`addonPath: addons/custom`).
25. Test: copertura ~33%, nessun test su `buildSyncScript`, validazioni, config rendering; l'e2e usa `kind latest` non pinnato.
26. `Dockerfile`: l'immagine dell'operatore include `git` che non serve più (il clone avviene nei Job); `alpine:3.20` è EOL → usare `gcr.io/distroless/static:nonroot`.
27. `manager.yaml`: `replicas: 1` con limite 128Mi; ok per partire, ma con molte istanze verificare memoria della cache (limitare la cache ai namespace gestiti con `cache.Options.DefaultNamespaces`).
28. `DEVELOPMENT_PLAN.md` descrive un operatore Python: documentazione non allineata (Go/Kubebuilder).

## 3. Pattern attuali e proposte di miglioramento

Fonti consultate: [Kubebuilder – good practices](https://book-v3.book.kubebuilder.io/reference/good-practices), guide pratiche Odoo-on-K8s ([Asergo](https://asergo.com/resources/deploying-odoo-on-kubernetes), [DeployMonkey](https://deploymonkey.com/blog/deploy-odoo-kubernetes-guide), [Cybrosys](https://www.cybrosys.com/blog/a-simple-guide-to-kubernetes-deployment-for-odoo-projects)). Le ricerche web non hanno restituito documentazione ufficiale recente su gevent/RWX: i punti marcati (*) sono basati su esperienza consolidata e vanno validati sulla vostra versione di Odoo.

### 3.1 Pattern operator
- **Conditions + observedGeneration** standard (`metav1.Condition`) e stampa in `kubectl get`.
- **Server-Side Apply** per le risorse figlie; `Owns()` con predicate `GenerationChangedPredicate` sul CR per evitare reconcile da soli cambi di status.
- **Validazione nel CRD con CEL** (`x-kubernetes-validations`, `+kubebuilder:validation:XValidation`): immutabilità di `instanceRef`, regole su `gitUrl`/`gitRef`, divieto chiavi in `config`. Elimina la dipendenza da webhook + cert-manager e il codice duplicato. Webhook solo se serve logica non esprimibile in CEL.
- **Reconcile non bloccante**, `MaxConcurrentReconciles` configurabile, `EventRecorder`, metriche custom.
- **Finalizer con significato** (retention PVC, cleanup moduli) oppure rimuoverli.
- Versionare l'API come `v1alpha1`→`v1beta1` finché i campi non sono stabili, con conversione quando cambia.

### 3.2 Pattern Odoo su Kubernetes
- (*) **Moduli custom nell'immagine (immutabile)** come strada primaria: CI costruisce `odoo:<ver>` + addons a SHA pinnato; l'operatore fa solo rollout. Garantisce rollback, riproducibilità, nessun PVC RWO condiviso, nessun sync a runtime. Il sync da Git resta per dev/staging (o con `initContainer` che clona in `emptyDir` per pod, evitando RWO condiviso).
- **Filestore**: RWX (CephFS/NFS/EFS/Azure Files) oppure storage S3 (modulo `ir_attachment_s3`/equivalente) per più repliche; RWO solo con `replicas: 1`.
- **Web / cron / websocket separati**: Deployment HTTP (`workers`, `proxy_mode`), Deployment cron (`--no-http`), routing `/websocket` → 8072; esporre via Ingress o Gateway API.
- **Job di lifecycle** come CR/azioni dedicate: `OdooDatabaseInit`, `OdooModuleUpgrade` (`odoo -u mod1,mod2 --stop-after-init`) eseguito *prima* del rollout (pre-upgrade Job, con lock per DB), più `OdooBackup`/`OdooRestore` (pg_dump/CNPG backup + snapshot filestore).
- **PostgreSQL**: integrazione con **CloudNativePG** (riferimento al Secret `-app`, `sslmode=verify-full` con CA, pooler PgBouncer in `transaction` mode non compatibile con alcune feature Odoo → usare `session`).
- **Sicurezza**: `readOnlyRootFilesystem` (+ `emptyDir` su `/tmp`), `automountServiceAccountToken: false`, ServiceAccount dedicato, NetworkPolicy per istanza (ingress solo da ingress-controller, egress solo DB/DNS), `admin_passwd` da Secret, `list_db=False`, pull da digest, ImagePullSecrets.
- **Risorse/Scheduling**: probe su `/web/health`(*), PDB, topology spread, HPA/KEDA su CPU o coda richieste, requests/limits dimensionati su `workers`.
- **Multi-istanza**: label standard `app.kubernetes.io/*`, quota/LimitRange per namespace, `OdooInstanceClass`/template condiviso per default di fleet, hibernation (`spec.suspend: true` → replicas 0).

## 4. Roadmap proposta

**P0 – prima di andare in produzione**
1. Retention PVC / niente ownerRef sui dati (#1)
2. Hash di config (e Secret) nel pod template (#2, #17)
3. Probe, PDB, strategy, porta 8072, `proxy_mode`/`workers`/`list_db`/`admin_passwd` da Secret (#7, #12, #13)
4. Credenziali Git via Secret (token/SSH + known_hosts), pinning SHA (#5, #10)
5. Sync atomico (directory versionata + symlink swap) e rollout/aggiornamento moduli al cambio commit (#3, #4)
6. Decidere il modello di storage per `replicas > 1` (RWX o addons-in-image) (#6)

**P1 – robustezza**
7. Reconcile non bloccante + `ls-remote` (#8, #9); cleanup moduli rimossi / conflitti `moduleName` (#11)
8. Conditions, eventi, metriche; SSA (#20–#22)
9. CEL al posto di webhook; rimuovere campi inutilizzati o implementarli (#19, #23)
10. Web/cron separati; `OdooModuleUpgrade` e backup CR (§3.2)
11. Hardening Job e immagine operatore (distroless), pin per digest (#16, #26)

**P2 – supply chain e qualità**
12. Dependabot/Renovate (gomod, docker, actions), `govulncheck`, scan immagine (Trivy), SBOM e firma (cosign), tag semver nella pipeline `build.yml`
13. Test: unit su validazioni/script/config, envtest su reconcile Job, e2e su kind con versione pinnata
14. Aggiornare README / `DEVELOPMENT_PLAN.md`; documentare matrice di compatibilità (Odoo 17/18/19, K8s ≥ 1.33)
