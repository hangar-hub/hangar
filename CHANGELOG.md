# Changelog

## [1.3.1](https://github.com/hangar-hub/hangar/compare/v1.3.0...v1.3.1) (2026-10-26)


### Bug fixes

* apply the language review to error messages, CLI help and UI copy ([#63](https://github.com/hangar-hub/hangar/issues/63)) ([fc8f915](https://github.com/hangar-hub/hangar/commit/fc8f9151a2b228570ed1f6fd4ef5468b58f2377b))
* **auth:** harden device login, sign-in lockout, Ollama keys and sessions ([#59](https://github.com/hangar-hub/hangar/issues/59)) ([d710aa1](https://github.com/hangar-hub/hangar/commit/d710aa12e7c7161244ebba616c9ae7ec0947408e))
* Hub API errors for Mirrors, commit limits and cursors, and graceful shutdown ([#62](https://github.com/hangar-hub/hangar/issues/62)) ([41feca3](https://github.com/hangar-hub/hangar/commit/41feca361d435e6eea94ad06ac30fbe8b545d483))
* keep staged uploads from GC, resume retried Pulls, and harden blob deletes ([#61](https://github.com/hangar-hub/hangar/issues/61)) ([5229d29](https://github.com/hangar-hub/hangar/commit/5229d29b35d395e3d4207a440c1a5854a92a034e))
* scan nested pickles, stop printing generated secrets, and scan release images before signing ([#60](https://github.com/hangar-hub/hangar/issues/60)) ([e660f0d](https://github.com/hangar-hub/hangar/commit/e660f0d8869e0706bcc9a30046f96f08b50ad9cb))


### Documentation

* add guides for an Ollama registry, a shared cache, CI and Kubernetes, and a banner on older versions ([#54](https://github.com/hangar-hub/hangar/issues/54)) ([11f1b41](https://github.com/hangar-hub/hangar/commit/11f1b4151519b943300a467bcb2ffead57d452cf))

## [1.3.0](https://github.com/hangar-hub/hangar/compare/v1.2.3...v1.3.0) (2026-10-23)


### Bug fixes

* bind OIDC to the browser and harden sign-in, Ollama keys, webhooks and dataset conversion ([#40](https://github.com/hangar-hub/hangar/issues/40)) ([7c62055](https://github.com/hangar-hub/hangar/commit/7c6205544ba3f0d5cb6083652ab2c18f13367679))
* cut stalled transfers and idle connections, stream JSON datasets and list non-ASCII trees ([#42](https://github.com/hangar-hub/hangar/issues/42)) ([c37484d](https://github.com/hangar-hub/hangar/commit/c37484d18b135cd5254f3ce1e3dab41f3828bb09))
* **import:** upload duplicate files once, fill Mirrors at the matched Commit and match the cache by content ([#41](https://github.com/hangar-hub/hangar/issues/41)) ([65196cd](https://github.com/hangar-hub/hangar/commit/65196cdd4ad0decbeb563da8ac040eb3614a93ac)), closes [#29](https://github.com/hangar-hub/hangar/issues/29)
* **server:** name the Blob store wait on /readyz from the first answer ([#49](https://github.com/hangar-hub/hangar/issues/49)) ([24dbd81](https://github.com/hangar-hub/hangar/commit/24dbd81ac58a5de612e642f213332b3d2dc30332))
* stop retried writes and garbage collection losing packs and LFS objects ([#39](https://github.com/hangar-hub/hangar/issues/39)) ([3b11b84](https://github.com/hangar-hub/hangar/commit/3b11b8468e1c4013721d4688a3b319cf230591d6))
* **ui:** stop editors overwriting Commits, enforce the external images setting and fix accessibility gaps ([#43](https://github.com/hangar-hub/hangar/issues/43)) ([03e65ca](https://github.com/hangar-hub/hangar/commit/03e65caf18e6078c86e6fee4b0bf5b288ad16979))


### Performance

* list Repos with a fixed number of queries, measured at 100,000 Repos ([#46](https://github.com/hangar-hub/hangar/issues/46)) ([f0f1051](https://github.com/hangar-hub/hangar/commit/f0f105136eac5316a90e9ae676cbc8d20675ed2a))


### Documentation

* one positioning sentence, new evaluator pages and corrected statements for launch ([#44](https://github.com/hangar-hub/hangar/issues/44)) ([0c7165a](https://github.com/hangar-hub/hangar/commit/0c7165a9b194ce8b621e13f4b7970b300ae99146))
* rework the README, add support files, and publish install notes and Helm metadata ([#45](https://github.com/hangar-hub/hangar/issues/45)) ([9ef06a4](https://github.com/hangar-hub/hangar/commit/9ef06a4d5baee02e66da088503ee5e1cb9f105a9))


### Dependencies

* bump certifi from 2026.7.22 to 2026.10.19 in /compat ([#36](https://github.com/hangar-hub/hangar/issues/36)) ([4b142d3](https://github.com/hangar-hub/hangar/commit/4b142d3774a0cc2f167da8de0ff8f5c5540f2084))

## [1.2.3](https://github.com/hangar-hub/hangar/compare/v1.2.2...v1.2.3) (2026-10-21)


### Documentation

* describe Hangar as a model and dataset store, and answer whether it is open source ([#37](https://github.com/hangar-hub/hangar/issues/37)) ([751a1a5](https://github.com/hangar-hub/hangar/commit/751a1a578630b4458a10382f4862ad927b2460f7))

## [1.2.2](https://github.com/hangar-hub/hangar/compare/v1.2.1...v1.2.2) (2026-10-21)


### Bug fixes

* build with Go 1.27.3 for the crypto/x509 fix in GO-2026-6809 ([#35](https://github.com/hangar-hub/hangar/issues/35)) ([fda6848](https://github.com/hangar-hub/hangar/commit/fda68485e10daea6bb8f0417d4502e0994b86b69))
* technical SEO fixes for hangar.page, the docs and the demo ([#32](https://github.com/hangar-hub/hangar/issues/32)) ([e479319](https://github.com/hangar-hub/hangar/commit/e47931980b7f623c639f24c99fec94106df88515))


### Documentation

* target private Hugging Face Hub searches, and compare Hangar with alternatives ([#33](https://github.com/hangar-hub/hangar/issues/33)) ([8c62f15](https://github.com/hangar-hub/hangar/commit/8c62f152b333e1d773b375871bc2886e4e41ddd5))

## [1.2.1](https://github.com/hangar-hub/hangar/compare/v1.2.0...v1.2.1) (2026-10-20)


### Bug fixes

* **docs:** give every docs page its own search description, and name the sites for Google ([#30](https://github.com/hangar-hub/hangar/issues/30)) ([938f091](https://github.com/hangar-hub/hangar/commit/938f09163f8bef6ca4227c2d2ea6776bc285fef0))

## [1.2.0](https://github.com/hangar-hub/hangar/compare/v1.1.0...v1.2.0) (2026-10-20)


### Features

* **ui:** explain badges on hover and click, with ui.docs_url for their docs links ([#24](https://github.com/hangar-hub/hangar/issues/24)) ([2e3bfba](https://github.com/hangar-hub/hangar/commit/2e3bfbaa241305d48409e60eef9a04ba7691aabd))
* verify with Bing and improve search ranking for all three sites ([#23](https://github.com/hangar-hub/hangar/issues/23)) ([c588b7d](https://github.com/hangar-hub/hangar/commit/c588b7d04853899a1bded21089d1dab63ce1a0f9))


### Bug fixes

* **cli:** import files from huggingface_hub 2.x's shared blob store ([#27](https://github.com/hangar-hub/hangar/issues/27)) ([ebcabed](https://github.com/hangar-hub/hangar/commit/ebcabed95fef4a2b9bf632b8287c4423df9b1130))
* **demo:** index the demo home page for crawlers, and add raster favicons ([#25](https://github.com/hangar-hub/hangar/issues/25)) ([c6589c8](https://github.com/hangar-hub/hangar/commit/c6589c83e15dfa09e14af8eb2a8001f483226eac))
* **docs:** link the docs logo to the home page ([#26](https://github.com/hangar-hub/hangar/issues/26)) ([2d98f0a](https://github.com/hangar-hub/hangar/commit/2d98f0ad1d77ed6d8bef5e94feaa94e6b9f550f0))
* **helm:** retry the helm test while the Service starts routing ([#21](https://github.com/hangar-hub/hangar/issues/21)) ([a57fffd](https://github.com/hangar-hub/hangar/commit/a57fffd7ea45496c63864687c86fe6f34d9da9b8))
* two races found by CI (S3 part ETag, gitstore Acquire) ([#14](https://github.com/hangar-hub/hangar/issues/14)) ([311e225](https://github.com/hangar-hub/hangar/commit/311e225f76f9d7e2edb3196337c3450bffa6fb07))


### Documentation

* **build:** record the v1.0.0 and v1.1.0 releases and the CI fixes after them ([#22](https://github.com/hangar-hub/hangar/issues/22)) ([144b290](https://github.com/hangar-hub/hangar/commit/144b29071a5f149d2229353b6ebb1c5342951982))


### Dependencies

* bump the go-minor-and-patch group across 1 directory with 2 updates ([#1](https://github.com/hangar-hub/hangar/issues/1)) ([4c84ac8](https://github.com/hangar-hub/hangar/commit/4c84ac8e1159000b71ecb5baef10c1510f428ac0))
* bump the ui-minor-and-patch group across 1 directory with 2 updates ([#2](https://github.com/hangar-hub/hangar/issues/2)) ([9b3d361](https://github.com/hangar-hub/hangar/commit/9b3d36124f81768afeb8643759eb250fa8ce7b4c))

## [1.1.0](https://github.com/hangar-hub/hangar/compare/v1.0.0...v1.1.0) (2026-10-19)


### Features

* **cli:** choose which models hangar import takes with --only ([#12](https://github.com/hangar-hub/hangar/issues/12)) ([ed4d21d](https://github.com/hangar-hub/hangar/commit/ed4d21d40f21dd97a2acdb5a469bfb3fac15bf9b))
* **docs:** add a home page at hangar.page ([#13](https://github.com/hangar-hub/hangar/issues/13)) ([2e1934f](https://github.com/hangar-hub/hangar/commit/2e1934f2d785e3495674cd0e943b1e1c805843a0))
* **docs:** animate the terminal on the home page ([#16](https://github.com/hangar-hub/hangar/issues/16)) ([a1ecd0e](https://github.com/hangar-hub/hangar/commit/a1ecd0edc590c5483fdff2c62e40029605ea4eb6))
* **docs:** say why Hangar exists on the home page ([#15](https://github.com/hangar-hub/hangar/issues/15)) ([51d20db](https://github.com/hangar-hub/hangar/commit/51d20dbf03686d5d79c8a94c9ae183ea55c8741b))
* **docs:** serve the docs from a Cloudflare Worker, with branch Previews ([#7](https://github.com/hangar-hub/hangar/issues/7)) ([6719480](https://github.com/hangar-hub/hangar/commit/671948011f0e7a7fa9fd9dc1b82b8ab07949288a))


### Bug fixes

* **jobs:** give an idle bandwidth slice back and take up freed budget sooner ([#19](https://github.com/hangar-hub/hangar/issues/19)) ([b70eaf3](https://github.com/hangar-hub/hangar/commit/b70eaf3d66c2400919a0c74d3bae0808d289fb77))
* **ui:** say when the admin's Every Token list is empty ([#10](https://github.com/hangar-hub/hangar/issues/10)) ([c8f0124](https://github.com/hangar-hub/hangar/commit/c8f01246b5691039bea8e983bba24d822814b7e0))
* **upstream:** stop asking the Hub for model-only fields on dataset search ([#17](https://github.com/hangar-hub/hangar/issues/17)) ([f8efceb](https://github.com/hangar-hub/hangar/commit/f8efceb88efb4f7e9acc7cdb6400942f2625ac9f))

## 1.0.0 (2026-10-18)


### Features

* **api:** add clock skew and configuration doctor checks ([d5c5158](https://github.com/hangar-hub/hangar/commit/d5c5158da3a4c9eff132c7aa4d1b8f4370a6143e))
* **api:** add Discover, Pulls, Mirrors, Dependencies and admin Upstream endpoints ([35af26d](https://github.com/hangar-hub/hangar/commit/35af26dc40e51b58b1ff70cda9f2113985893163))
* **api:** diff a pull request's files against its fork point ([c2b4dff](https://github.com/hangar-hub/hangar/commit/c2b4dff48e7cab3b51ea42aecd1476971d888ce2))
* **api:** expose ui.external_images on /auth/providers and document history-only Pulls ([5b4e8b3](https://github.com/hangar-hub/hangar/commit/5b4e8b3f90ba7013d33bb2caa59bd1195d347286))
* **api:** let users change their own profile and password ([38cb14a](https://github.com/hangar-hub/hangar/commit/38cb14aacb5c32ea7bfcfcaa65931571407795f8))
* **api:** list a file's scan findings for the file page ([2cea799](https://github.com/hangar-hub/hangar/commit/2cea799f2bbfc29ae4a5a979fb8d1876c666d8e1))
* **api:** list every Organisation for Instance admins ([766fdfa](https://github.com/hangar-hub/hangar/commit/766fdfacc737b6602870e8d377f368d77576e157))
* **api:** name the Repo in the payload of every Repo-scoped Job ([7f9f3c2](https://github.com/hangar-hub/hangar/commit/7f9f3c2ab3ca75977867a61e1a1f121b91a4387c))
* **api:** report the S3 upload mode on /admin/system and the System page ([a23e48e](https://github.com/hangar-hub/hangar/commit/a23e48ee1498cfd4f9b316a90923670668aa7cbc))
* **api:** serve metadata, discussions, Collections and webhooks in the native API ([3c24740](https://github.com/hangar-hub/hangar/commit/3c24740f92f5e476fa3dbbd24f3b10e7db0fb293))
* **auth:** require sign-in for Namespace profiles while anonymous_read is off ([81390e5](https://github.com/hangar-hub/hangar/commit/81390e5f1d3239bdfc510f0d5ded144424df69d7))
* **cli:** add --json to hangar logout and check every command has it ([4f9d363](https://github.com/hangar-hub/hangar/commit/4f9d363ad11613696f062810e53db8956ab495e3))
* **cli:** add the hangar client, import, LM Studio, Ollama and admin backup commands ([4c5ed59](https://github.com/hangar-hub/hangar/commit/4c5ed597074770c501c6eb67136468bb35f3f2b2))
* **cli:** check the quant is in the Arsenal before hangar ollama install ([e9c3747](https://github.com/hangar-hub/hangar/commit/e9c37471bc9159d401f895c838538e2c6554c8b1))
* **cli:** find a proxy that cuts long Blob store reads with doctor --large-read ([87f07c3](https://github.com/hangar-hub/hangar/commit/87f07c338db11e6c1f4f7acd01d765aa7ddf6af5))
* **cmd:** add admin create-user, reset-password, gc and reindex, and open the Blob store for backup, restore and doctor ([078923b](https://github.com/hangar-hub/hangar/commit/078923b90669d4006b987313eeba18c610d83f1f))
* **cmd:** take --json on the server and admin commands ([dbe58b2](https://github.com/hangar-hub/hangar/commit/dbe58b24862eb4c94b47d7a067765553103e1b93))
* **cmd:** wire every workstream's real implementation into hangar server and worker ([05fed11](https://github.com/hangar-hub/hangar/commit/05fed112de6bcef458294a363e165245d74b7912))
* **collections:** store Collections with visibility-aware items ([c1efec3](https://github.com/hangar-hub/hangar/commit/c1efec3f9fa74226718f264cf603f3879affe7ac))
* **community:** store discussions, pull requests, likes and download counts ([3a272bb](https://github.com/hangar-hub/hangar/commit/3a272bbe08c6bf67e2d3d63dd3cded01a55f1de8))
* **config:** add viewer.max_request_bytes for the Dataset Viewer budget ([594f1ea](https://github.com/hangar-hub/hangar/commit/594f1eab4e19f31679b4e2bb32ce9fbc31b1ce6e))
* **datasets:** Dataset Viewer API, signed media assets, Croissant and dataset metadata ([19e04cd](https://github.com/hangar-hub/hangar/commit/19e04cda87fca5df618b72022808e41b1a2c1ed5))
* **datasets:** dataset.convert and dataset.stats Jobs with the refs/convert/parquet export ([18f61cf](https://github.com/hangar-hub/hangar/commit/18f61cffe06e67c2ec1fded920072b25665e0c0e))
* **datasets:** features, layout, Arrow IPC and Parquet IO, conversion readers and statistics ([85fcb11](https://github.com/hangar-hub/hangar/commit/85fcb112fa6e8496b29832e97700d04193b89838))
* **datasets:** native configs endpoint and compat/viewer suite ([717f386](https://github.com/hangar-hub/hangar/commit/717f386c9b8d0b8bfee8e46284995b7d0c60df9e))
* **datasets:** where and orderby parser and evaluator for /filter, with fuzz targets ([d0ef69a](https://github.com/hangar-hub/hangar/commit/d0ef69afdfd3ac5a237037682cfad2fedd572943))
* **deploy:** add Grafana and Kibana dashboards, load tests and protocol labels on HTTP metrics ([ed09aa5](https://github.com/hangar-hub/hangar/commit/ed09aa5c4ed06eb531507e57e6ca96674bd96a99))
* **deploy:** add the Compose examples and the smoke test scripts ([dc6d178](https://github.com/hangar-hub/hangar/commit/dc6d178d185354d3d148454259bc7e71e682da63))
* **deploy:** add the container image, GoReleaser config, Helm chart and LM Studio compat suite ([0a33964](https://github.com/hangar-hub/hangar/commit/0a339640fa930cae4804be2abda60bde44086c64))
* **hfapi:** enforce scan.block_unsafe_downloads on Hub API downloads ([93f90b8](https://github.com/hangar-hub/hangar/commit/93f90b803c7513ce077ddce6188ff7f8391e8c96))
* **hfapi:** serve liker listings and comment edit and hide ([aebc1ca](https://github.com/hangar-hub/hangar/commit/aebc1caf4d7c55cf7dab770cc67bcd6808e999af))
* **index:** index Commits with ranged safetensors and GGUF header reads ([467ff6e](https://github.com/hangar-hub/hangar/commit/467ff6e5ced6f994a6de4256e2417ab4fb2167b6))
* **index:** record a dataset's rows and size category from its Parquet export ([962f32a](https://github.com/hangar-hub/hangar/commit/962f32ad79ddc2d5f7e0184f33f2c8407e6e9f5f))
* **index:** store rows, bytes and columns per Split and link datasets with models ([2f6f88e](https://github.com/hangar-hub/hangar/commit/2f6f88ecf8db91c0fbdc1c0df867e7996b203b8e))
* integration hooks for Pull-through, mirror status, safe downloads and admin password resets ([e3e0c0e](https://github.com/hangar-hub/hangar/commit/e3e0c0ef5cb29b9f822252f03dbfaed0fda842ad))
* **ollama:** serve GGUF Repos as an Ollama registry ([f970124](https://github.com/hangar-hub/hangar/commit/f9701246393db65528d64a15a40a6ef66869b970))
* **repo:** check LFS object references at Commit ([64fd267](https://github.com/hangar-hub/hangar/commit/64fd26788330136746810955846f5f7a11733f21))
* **repo:** filter Repo listings by dataset row count ([e2f4ef8](https://github.com/hangar-hub/hangar/commit/e2f4ef80cb8b6b4e573f00a97d65dbf4f76603c3))
* **repo:** wait at most lfs.verify_wait for LFS verification in Commits and pushes ([ee83f5b](https://github.com/hangar-hub/hangar/commit/ee83f5ba0600bb7e5f4c70c9c190b8188e3f220b))
* **scan:** make the download gate's level a setting ([e1b57c2](https://github.com/hangar-hub/hangar/commit/e1b57c27998f442f0cb3d9064c3e33daaa3da3b5))
* **scan:** scan pickles, PyTorch archives and integrity of model files ([4a9d5ae](https://github.com/hangar-hub/hangar/commit/4a9d5ae8cf9aa182c7c878978fd08942569e0e50))
* scope Repo Jobs, add auth.cleanup and Namespace search for the integration ([c4790b4](https://github.com/hangar-hub/hangar/commit/c4790b4cee89ba564ea402aecfbcb6915ea40978))
* **server:** wait for the Blob store at start instead of exiting ([22eded4](https://github.com/hangar-hub/hangar/commit/22eded40bdb2a00cda0305d1baf39c2443cee81b))
* **ui:** add the mock demo for demo.hangar.page as a Cloudflare Worker ([df87593](https://github.com/hangar-hub/hangar/commit/df875939218a9bdd89f7637ce2facab4ccdef67a))
* **ui:** build every page against the native API with an OpenAPI mock server ([962614a](https://github.com/hangar-hub/hangar/commit/962614a500e092bdeda7b7e34c5f355e1a94f873))
* **ui:** edit the profile and change the password in Settings ([a0706a9](https://github.com/hangar-hub/hangar/commit/a0706a9f105bfe1be275235975f9fde4ba443de8))
* **ui:** polish the interface with motion, shared primitives and consistent details ([a670c48](https://github.com/hangar-hub/hangar/commit/a670c4865c2e82af6c6084acf5e612471eb819c4))
* **ui:** separate Models and Datasets sections, Taxiway teal accent and optional dataset links ([6fd838a](https://github.com/hangar-hub/hangar/commit/6fd838a608fe99da37e342ef970d3006f102f4ef))
* **ui:** show pending emails and confirm them from their link ([30d1998](https://github.com/hangar-hub/hangar/commit/30d199882d8ee7e5b5408084b5a5ca4701d7edbb))
* **uploads:** continue the upload in progress when a client plans again ([e114f75](https://github.com/hangar-hub/hangar/commit/e114f75eab9f446b0bdc9ccd263f14e33ae9a66f))
* **upstream:** add bulk import from a Hugging Face cache, LM Studio, a directory and Ollama ([0e3c166](https://github.com/hangar-hub/hangar/commit/0e3c166e247c6481faafd5eba1d108b037ef25a3))
* **upstream:** add the Upstream client, registry, Pull planner, transfers, Pull-through and Discover ([2cc3a0d](https://github.com/hangar-hub/hangar/commit/2cc3a0d1e651bfccc76f6ae686dd451d0638e241))
* **webhook:** store webhooks and deliver signed events with retries ([3ce11ad](https://github.com/hangar-hub/hangar/commit/3ce11ad516387c83915b3c9954d6bcda962f7074))
* **web:** send Cross-Origin-Opener-Policy and Permissions-Policy with UI pages ([4b609f3](https://github.com/hangar-hub/hangar/commit/4b609f39f4fb1ab05b55fa60188d395ea7b9366d))
* **xet:** ingest Xet uploads and reassemble them into LFS objects ([ee75dcc](https://github.com/hangar-hub/hangar/commit/ee75dcc08a35e74e9f9b0867a5fd77d7c3db8936))
* **xet:** parse and verify xorbs and shards against the Xet reference vectors ([3af7bc0](https://github.com/hangar-hub/hangar/commit/3af7bc0188f7596379ac56383b0aa01e2587ed30))


### Bug fixes

* answer 499 to client-closed requests in the Viewer, Ollama and sign-in writers ([9d8d29d](https://github.com/hangar-hub/hangar/commit/9d8d29de56572b6939a2efbf5788f1e825766b38))
* **api:** answer 499 to client-closed requests in every error writer ([6c93d86](https://github.com/hangar-hub/hangar/commit/6c93d8607beb0af76caa2c0db2c8ee7eac453b2a))
* **api:** carry scan results forward while a Commit's scan is pending ([1622a27](https://github.com/hangar-hub/hangar/commit/1622a27645d8b288f7bee151b012fa88c3c6fea5))
* **api:** check Repo access before showing or changing a Job ([18191e4](https://github.com/hangar-hub/hangar/commit/18191e40c923123f66bec40d2299fb43bc5ce80b))
* **api:** keep gating keys a Repo settings update does not send ([8715a05](https://github.com/hangar-hub/hangar/commit/8715a058062fa0ced1c306bec9be465fe06e3467))
* **api:** make the Mirror update check budget explicit and log a missed check ([1430d64](https://github.com/hangar-hub/hangar/commit/1430d640cf4f8fb0db022a405366a04466720815))
* **api:** stop a Token creating a Token that outlives it ([5199150](https://github.com/hangar-hub/hangar/commit/51991508d9244f429f140b6a49c35a99f196eac8))
* **auth:** answer a client-closed request with 499 and log authentication store errors ([6caf784](https://github.com/hangar-hub/hangar/commit/6caf784d0333fe5e1d624a5605790f3bdbdb16d1))
* **auth:** answer sign-in throttling with a sentence users can read ([7369255](https://github.com/hangar-hub/hangar/commit/73692559517c92f79d5d833c0c5f5506b55ac666))
* **auth:** bind email confirmation links to the address the admin saw ([c937c8e](https://github.com/hangar-hub/hangar/commit/c937c8e09deb1b90514ab68333968474ccf7f1d6))
* **auth:** bound concurrent password hashing to stop sign-in floods exhausting memory ([89631fa](https://github.com/hangar-hub/hangar/commit/89631fada8bd8ad9a3398c4456504d87f3ee0c09))
* **auth:** let a write Token administer Repos its user administers ([54e5307](https://github.com/hangar-hub/hangar/commit/54e5307b3bfec7365bd10cecae91a6042823552d))
* **auth:** let a write Token administer Repos only through the Hub API ([425a744](https://github.com/hangar-hub/hangar/commit/425a744bc348fef30aa6de84816e8f89950501a9))
* **auth:** make self-set emails wait for a confirmation link ([f9f6421](https://github.com/hangar-hub/hangar/commit/f9f6421081d6e511ee79bb2e9f48939d2cc1ce95))
* **auth:** refuse cross-origin changes made with browser credentials ([5ad144f](https://github.com/hangar-hub/hangar/commit/5ad144f1b891a798c2ace8ece5839ccc484df2c9))
* **auth:** treat a stale session cookie as anonymous and clear it ([53a90a9](https://github.com/hangar-hub/hangar/commit/53a90a94cf6907a697787ace5ae2aa58bbfa8912))
* **blob:** copy large S3 objects server-side in 512 MiB parts ([7972a2c](https://github.com/hangar-hub/hangar/commit/7972a2c750cc7e4d919ac3e75bbdd046f35dd38e))
* **blob:** fail a filesystem install at once when the key's path is a directory ([1355a1f](https://github.com/hangar-hub/hangar/commit/1355a1f3a2bb3cd49f5783de35451656618b4a8b))
* **blob:** record sanitised exception events on Blob store spans ([a917701](https://github.com/hangar-hub/hangar/commit/a9177012120a49b650902cc364651252890fe119))
* **blob:** retry S3 uploads over slow links and report a missing bucket ([8f8f3dc](https://github.com/hangar-hub/hangar/commit/8f8f3dc2e25a790691d530b1a04ffaa8478b0d53))
* **blob:** serve every download as an octet-stream attachment ([e76e619](https://github.com/hangar-hub/hangar/commit/e76e6192c300ace75a462a0a4aa3c26479d9e302))
* CI after the interface polish ([#6](https://github.com/hangar-hub/hangar/issues/6)) ([d09e7af](https://github.com/hangar-hub/hangar/commit/d09e7af5c283f6ebeb8723b59f191381544de9c9))
* **cli:** create the branch hangar push --branch names when it does not exist ([320939d](https://github.com/hangar-hub/hangar/commit/320939d19599f6f5b1edcf8698d4b07e0829a925))
* **cli:** follow links to Hugging Face cache blobs when uploading a folder ([f7c80a7](https://github.com/hangar-hub/hangar/commit/f7c80a706a569037271008c8b85451cffccc1283))
* **cli:** give every command a help heading in sentence case ([73b7664](https://github.com/hangar-hub/hangar/commit/73b7664c2988672c3ff9f91cee929944fd5a9ec1))
* **cli:** keep presigned URLs out of error output and drop the Token on cross-origin redirects ([9aeefd2](https://github.com/hangar-hub/hangar/commit/9aeefd23d4087fb8197885f700b2660eaa915259))
* **cli:** leave other quants out when GGUF files are regular blobs ([b0624c4](https://github.com/hangar-hub/hangar/commit/b0624c42f732beddb6abf3366e9e1f34d8f66de2))
* **cli:** never upload files that symbolic links point to outside the folder ([9cf64d1](https://github.com/hangar-hub/hangar/commit/9cf64d1b52509a2a91f8f7b05b71db07c8ff6af6))
* **cli:** pull imported files into the Mirror so the Instance marks them present ([c786af3](https://github.com/hangar-hub/hangar/commit/c786af3a8756b672ba83c8f709fe8789711a6d96))
* **cli:** put every subcommand's help headings in sentence case ([87cbc4d](https://github.com/hangar-hub/hangar/commit/87cbc4dfd15d94d32cc6350fa1eaf146e40ad0ea))
* **cli:** read the generated Instance key in hangar admin doctor ([0ca4552](https://github.com/hangar-hub/hangar/commit/0ca45528c67ddc46fbe1c8ab9827b9711e2dea90))
* **cli:** retry part uploads and downloads, and say what an import waits for ([66a9488](https://github.com/hangar-hub/hangar/commit/66a94880c1f2afccb38c468cbf40ca9b7c044c84))
* **cli:** say what an upload completion waits for and scale its timeout ([8a78d5c](https://github.com/hangar-hub/hangar/commit/8a78d5cbff2c9f0fd818c498447e3878a9aec88d))
* **cli:** say when Ollama needs the key in hangar ollama install ([23fd187](https://github.com/hangar-hub/hangar/commit/23fd1877d085bb2847e720fd4407ac62bffc834f))
* **cli:** send an upload completion again after 503 with Retry-After ([071ee7d](https://github.com/hangar-hub/hangar/commit/071ee7d50311f4f75219629869d0c46f03f4dd97))
* **cli:** upload the other files when one fails after its retries ([1d0e599](https://github.com/hangar-hub/hangar/commit/1d0e599bb9558a5f1449daf35664f074ab92c7c2))
* **compat/hf:** pass AnonymousRead as a setting func in the real backend ([148ab3b](https://github.com/hangar-hub/hangar/commit/148ab3b726a91b7772d2c71e992c18089b7eab33))
* **compat:** pass AnonymousRead as a function in the Xet real backend ([bad5714](https://github.com/hangar-hub/hangar/commit/bad571437aa137e1e618ba57cbb0c0cfa30e2e45))
* **compat:** remove the hangar binary the harness builds when the tests end ([1e0b1cb](https://github.com/hangar-hub/hangar/commit/1e0b1cb4e74cbcb53174591f813ac904edcf09a7))
* **compat:** retry and verify the viewer fixture downloads ([4a79651](https://github.com/hangar-hub/hangar/commit/4a79651ddebe6588a28872d219aa48918e7dee59))
* **config:** refuse a URL DSN with the sqlite driver ([04678aa](https://github.com/hangar-hub/hangar/commit/04678aa8265105d5b4bff922dfc78fc55dfca81e))
* **datasets:** adopt a Mirror's Parquet export once it is pulled ([bb95881](https://github.com/hangar-hub/hangar/commit/bb958816dde6b38dadcc578fdf48ebcd7392994e))
* **datasets:** charge decoded Parquet data to a per-request budget ([3eb0655](https://github.com/hangar-hub/hangar/commit/3eb0655abaa1b5ec68b30fcd772acee826e501ae))
* **datasets:** check delta byte-array lengths and decimal scales before reading Parquet ([9fa386e](https://github.com/hangar-hub/hangar/commit/9fa386e9d28005d2af8e40ab652398884c6cb64b))
* **datasets:** check Parquet sizes and counts before parquet-go reads them ([4001461](https://github.com/hangar-hub/hangar/commit/4001461acb0be2004893da572fed7be4b6bbd132))
* **datasets:** log a dataset with no Configs at DEBUG in the conversion ([f210b05](https://github.com/hangar-hub/hangar/commit/f210b05f905a3bd76e4ba25794a278f184d2e9c9))
* **datasets:** read LZ4 frame compressed Arrow IPC buffers ([bda64e7](https://github.com/hangar-hub/hangar/commit/bda64e740127abd106c28ffe624e2cb380c541e0))
* **datasets:** read zip archives as datasets does and hide failure detail ([754a1a9](https://github.com/hangar-hub/hangar/commit/754a1a9b253cac5f9ac97eb96e027cd656cd6908))
* **datasets:** report source sizes for Mirrors that keep the Upstream's Parquet export ([a690d1d](https://github.com/hangar-hub/hangar/commit/a690d1dca1b859b990500841d4097ee4d865b04f))
* **datasets:** retry dataset Jobs on transient Repo read errors ([a8f4654](https://github.com/hangar-hub/hangar/commit/a8f46541552915ca0693e638fa93d62e36f6e8e2))
* **db:** make concurrent migrators wait on a Postgres advisory lock ([026bd0a](https://github.com/hangar-hub/hangar/commit/026bd0a9cd0e05b4fc232aa31c7cc843e3fdd460))
* **deploy:** define the scopes Hangar requests in the Keycloak realm ([ee4b2bb](https://github.com/hangar-hub/hangar/commit/ee4b2bb9172735077a1c67df333192198781e41d))
* **deploy:** let readiness probes outwait a bounded /readyz ([4b440b7](https://github.com/hangar-hub/hangar/commit/4b440b79b3d56452c29cf2d8f71c27f05fed1062))
* **deploy:** let the migrate init container wait for the database ([d237fa9](https://github.com/hangar-hub/hangar/commit/d237fa95427297f6338bc2bb6b18b81eee310779))
* **deploy:** make the Postgres health checks wait for TCP ([8daab65](https://github.com/hangar-hub/hangar/commit/8daab65fbd2a31c23766a31fe27233197408cd79))
* **deploy:** render GitOps installs without the helm test Pod ([9b1ca9b](https://github.com/hangar-hub/hangar/commit/9b1ca9b958ad0d64f51cf58939a48c2351c00483))
* **deploy:** require every Compose secret and keep admin ports on loopback ([ea7b194](https://github.com/hangar-hub/hangar/commit/ea7b1941c63558650475fd8b33ab2b6496c29ada))
* **deploy:** turn off service links in every chart Pod ([0d7a32d](https://github.com/hangar-hub/hangar/commit/0d7a32dda35e6f76188766b8060f4c3c2dc3ce5d))
* **docs:** cite each threat model test at its own file and drop spaces before percent signs ([8257431](https://github.com/hangar-hub/hangar/commit/8257431f2e92f51b96eef3a2da0a33e1519b3b92))
* **docs:** repair a broken spec link in decisions and check Markdown links ([8dbcf62](https://github.com/hangar-hub/hangar/commit/8dbcf6243dd182fd0a44b4b48e6e9308d3f99e86))
* **gc:** walk whole prefixes in orphan sweeps and sweep filesystem staging ([601e102](https://github.com/hangar-hub/hangar/commit/601e1024d6abe3af8bee8c7b6f4300342c269701))
* **git:** bound what pushed and pulled packs and trees can expand to ([dc3918b](https://github.com/hangar-hub/hangar/commit/dc3918b0dcc331b27293ae74f689a47190665d28))
* **gitserver:** name oversized files when two pushed objects share a first byte ([b0ddde7](https://github.com/hangar-hub/hangar/commit/b0ddde7f33ae0771a4a18e1e644c47d3addcf0e2))
* **gitserver:** refuse git fetches of refused files stored in git ([f65ba44](https://github.com/hangar-hub/hangar/commit/f65ba44ffa64b9fec488972508c9d16defa1c737))
* **gitstore:** report a cancelled context when git is killed mid-read ([56fd278](https://github.com/hangar-hub/hangar/commit/56fd27855beaa9b8f947cbaa78842a8dab4de327))
* **helm:** migrate in an init container for GitOps, refuse to generate secrets without cluster access, size the cache volume above the pack cache ([29d2073](https://github.com/hangar-hub/hangar/commit/29d20737db64e22294fdc5f95326c1b7f939e71f))
* **helm:** point NOTES at the generated password only when the chart generates it ([8a4f1a5](https://github.com/hangar-hub/hangar/commit/8a4f1a540a9233fc127b4ac08d821e68f5d1173c))
* **hfapi:** answer Pull-through policy refusals with 403 ([da5dfe5](https://github.com/hangar-hub/hangar/commit/da5dfe511ba248d57d68681b7dbee64f508210ab))
* **hfapi:** check a create_pr commit before opening its pull request ([8d197b1](https://github.com/hangar-hub/hangar/commit/8d197b16ff739cfc39b785dc78604d850fa0120b))
* **hfapi:** discard a create_pr pull request whose Commit failed ([b1dd72b](https://github.com/hangar-hub/hangar/commit/b1dd72b7ea1569264c427509c8a8956911b31e67))
* **hfapi:** issue no presigned link on HEAD for a download the gate refuses ([8fe5fd3](https://github.com/hangar-hub/hangar/commit/8fe5fd38ee69a9923a5c21177ed5295705ae27a5))
* **httpx:** keep a probe timeout out of the readiness cache and log failed checks ([2ad5361](https://github.com/hangar-hub/hangar/commit/2ad53616ae9e1948554aebde15b75f71e741c085))
* **httpx:** make the signed form of a URL unambiguous ([c1d1038](https://github.com/hangar-hub/hangar/commit/c1d103879751140079c42bd8611d597f49d08fde))
* **httpx:** name the request ID in server errors and on every request log line ([1c40413](https://github.com/hangar-hub/hangar/commit/1c40413794bec09b5ad0ae71e96a0bbc696dcbce))
* **image:** give admin backup a /backups folder it can write ([6cdba58](https://github.com/hangar-hub/hangar/commit/6cdba58a2f7c442fec15b8106fc6e9db4aa631fb))
* **index:** serve GGUF header metadata as plain values, the shape the UI reads ([ab5e5a5](https://github.com/hangar-hub/hangar/commit/ab5e5a5d87c1cc30272bfbdb98ef6c0a3d07d7e6))
* **jobs:** fence a run's writes on the attempt it claimed ([5e5c716](https://github.com/hangar-hub/hangar/commit/5e5c71649d16af77ef73b74299de81f6e5c3b3a7))
* **jobs:** fence Job writes on the attempt number as well as the lease owner ([d587bbb](https://github.com/hangar-hub/hangar/commit/d587bbbb9f09544b426c9d834e0ed02363c9ab1b))
* **jobs:** redact the whole query when it holds quotes or angle brackets ([f933253](https://github.com/hangar-hub/hangar/commit/f933253126fa1581797112acecc661f65884be19))
* **jobs:** redact URLs in Job errors and keep panic stacks in the log ([68672ed](https://github.com/hangar-hub/hangar/commit/68672edb3efdc5073ed057102d57c761c1e07f48))
* **loadtest:** make upload.js send the bytes it plans ([1044d4c](https://github.com/hangar-hub/hangar/commit/1044d4c767e358fa9fe2636d4d9c8bdf858ec0f3))
* **loadtest:** upload new bytes to one Repo per VU in upload.js ([2581fd6](https://github.com/hangar-hub/hangar/commit/2581fd6b5ee6d66406eac39dcc2f4af2e46a3df5))
* quieten health probes, create_repo on an existing Repo and the UI's first /me ([80fc37e](https://github.com/hangar-hub/hangar/commit/80fc37e951a393bb194ed0e021383c18aaea67b1))
* **release:** sign release checksums into cosign 3 bundles and rehearse OPS-7 locally ([0b39255](https://github.com/hangar-hub/hangar/commit/0b39255c4477cabf23cda74cbcad2f84650d9342))
* **repo:** apply the stored anonymous_read setting to Repo listing ([0d4eaa8](https://github.com/hangar-hub/hangar/commit/0d4eaa8e279705c00a54e68ee93f4eb1c322a6e4))
* **repo:** bound server-side Commits by the same tree limits as pushes ([aa8a2ce](https://github.com/hangar-hub/hangar/commit/aa8a2ce248c5460e27d406e36f5284071ef8547d))
* **repo:** finish an upload completion after its caller goes away ([a3e3502](https://github.com/hangar-hub/hangar/commit/a3e35023b54c30f36f855faa4695958221e23c83))
* **repo:** only Hangar commits to refs/convert/* ([0386845](https://github.com/hangar-hub/hangar/commit/0386845e41527b2bc8555211e4ebcdaf0a694577))
* **repo:** resume broken reads when completing an upload and keep its parts on failure ([2284992](https://github.com/hangar-hub/hangar/commit/2284992167234c5529ee6ac374b488b079b7c066))
* **repo:** retry concurrent Commits to one branch with jittered backoff ([bd2b887](https://github.com/hangar-hub/hangar/commit/bd2b8872e5640b0022327e278ce7a9cf40d80f32))
* **repo:** sign no headers on presigned LFS PUTs and promote verified uploads by copy ([6707273](https://github.com/hangar-hub/hangar/commit/67072738b5ea2d6fdb6074598c3f56920bee58a3))
* **repotest:** store LFS-tracked adds as LFS objects and map Namespace IDs, as the real service does ([555d9b7](https://github.com/hangar-hub/hangar/commit/555d9b787a7fc4cacd7650abdc2aea881ac5aa81))
* **scan:** end scan and index Jobs quietly when their Repo is deleted mid-run ([7f308b9](https://github.com/hangar-hub/hangar/commit/7f308b94a6d769002f41499156c356334546d0e4))
* **scan:** enforce scan.block_unsafe_downloads on every download route ([7208565](https://github.com/hangar-hub/hangar/commit/7208565c6fa19965fcabb1aa5f368d047a969a00))
* **scan:** fail closed for object downloads that no scanned file covers ([cc8e372](https://github.com/hangar-hub/hangar/commit/cc8e372e9ce832f69e8a33912418a424716c735a))
* **scan:** gate signed Pull-through and zip export links, and enumerate every download path ([ae23e84](https://github.com/hangar-hub/hangar/commit/ae23e84aac49bbfd4feda5b6c89328783cd47798))
* **scan:** read files by content and fail closed on partial scans ([4e62696](https://github.com/hangar-hub/hangar/commit/4e62696532d5da8944600d0e038342213a0a8f45))
* **scan:** read pickles as CPython and joblib do ([bdcd21b](https://github.com/hangar-hub/hangar/commit/bdcd21b18e023066a2534dcda501bc4b376975d9))
* **scripts:** clone without credential helpers in the smoke test ([059f5ff](https://github.com/hangar-hub/hangar/commit/059f5ff23d8cb6a73440659f4831aa33fb628bd1))
* **scripts:** fail kind-chart-test.sh at once when the Hangar image is not local ([cebfa8f](https://github.com/hangar-hub/hangar/commit/cebfa8fead61b02bec37ce56b8b2b2bab44486d5))
* **scripts:** read fuzz targets line by line so shellcheck passes ([2b4d8b4](https://github.com/hangar-hub/hangar/commit/2b4d8b42d0899bb34662e9ea0f186e61fc27e93a))
* **scripts:** remove the anonymous volumes test containers leave behind ([8c1610c](https://github.com/hangar-hub/hangar/commit/8c1610cb5d372ea16a1561a6179ae6ad893534f5))
* **server:** keep /readyz at 503 until every route is mounted ([25a6ce8](https://github.com/hangar-hub/hangar/commit/25a6ce8b0bc6812f00bac53ffd78e2bf9f0d39a0))
* **server:** keep /readyz at 503 until the full handler is installed ([621f734](https://github.com/hangar-hub/hangar/commit/621f73418f0b59f70a44ef2ef6760f7292c2d0e2))
* **server:** name the Blob store in /readyz only when start-up waits for it ([36e4444](https://github.com/hangar-hub/hangar/commit/36e4444aade5e2edddd3094e70e495f04ab50b1f))
* **server:** report not ready until the Instance serves ([0e2c605](https://github.com/hangar-hub/hangar/commit/0e2c60557130227fd3ffcfddafaaed003ae815e3))
* **server:** report ready only once every route is served ([a772e09](https://github.com/hangar-hub/hangar/commit/a772e09bd57511e6b2157ae030c673dddedd6b95))
* **storage:** add storage.s3.upload_mode proxy for buckets clients cannot reach ([62c1181](https://github.com/hangar-hub/hangar/commit/62c11814a699233f397dcba88647f098e5e0ab38))
* test failures from the first public CI run ([#4](https://github.com/hangar-hub/hangar/issues/4)) ([39761a4](https://github.com/hangar-hub/hangar/commit/39761a4c20224bc5c6f37698fea5281a189916d8))
* **ui:** call the owner part of a Repo ID a Namespace, as CONTEXT.md asks ([25320df](https://github.com/hangar-hub/hangar/commit/25320df1170134b9785d1eaa3acc1328ad3ab4a0))
* **ui:** describe Token scopes as the Hub-actions-only decision does ([c4fc196](https://github.com/hangar-hub/hangar/commit/c4fc196a724ac6ec096860e5bb7192f40f0e3896))
* **ui:** focus the search input on open, wrap long IDs and scroll Markdown tables in focusable regions ([45eca07](https://github.com/hangar-hub/hangar/commit/45eca07719a0ee8679522972e5db52959ed3591c))
* **ui:** keep the search palette's listbox valid while results load ([46fa2a3](https://github.com/hangar-hub/hangar/commit/46fa2a39353a0de1ab6ae8e82847b59c283df9e5))
* **ui:** match the docs in Discover, the landing page and Token scopes ([4f57f3f](https://github.com/hangar-hub/hangar/commit/4f57f3ff669d505d72d6e7eba6cf79383be345c4))
* **ui:** nest Markdown headings under the page's h1 ([874ff55](https://github.com/hangar-hub/hangar/commit/874ff55c97b06573b97542426115dc247ca502fa))
* **ui:** resume browser uploads after a reload when the plan starts a new upload ([13121dc](https://github.com/hangar-hub/hangar/commit/13121dc98cf15f8cd3f8138dd8eb4fee2d1fca45))
* **ui:** stop pages scrolling sideways and let the keyboard scroll Markdown code ([4a03a21](https://github.com/hangar-hub/hangar/commit/4a03a21e19071a35591e160f6f2a290a6bf6e716))
* **ui:** stop the mock's OIDC sign-in redirecting to other sites ([9229225](https://github.com/hangar-hub/hangar/commit/922922540364f1386160a7216d9855952d506009))
* **ui:** use the hangar lmstudio install syntax from 08 in the LM Studio snippet ([88f76f9](https://github.com/hangar-hub/hangar/commit/88f76f9c1eb3a9fee5f3925516d4f6eec8ac958a))
* **upstream:** bound the refs read from an Upstream's advertisement ([8bf25b8](https://github.com/hangar-hub/hangar/commit/8bf25b8d710a0701d1506229786b74f1918f0a23))
* **upstream:** enforce Namespace quotas on Pulls ([6469737](https://github.com/hangar-hub/hangar/commit/64697372d452e5d5ee591f177e61a8b9bbd583d3))
* **upstream:** keep another Namespace's quota refusal out of joined Pulls ([1bdb098](https://github.com/hangar-hub/hangar/commit/1bdb0986e63b3a8c861b2cb0cd5a91bf672b18e8))
* **upstream:** make the Upstream connection test one bounded attempt ([4f0af00](https://github.com/hangar-hub/hangar/commit/4f0af00343c0800167c079871bbd1b02ce8e5d93))
* **upstream:** plan a Pull again at once when another run stored its history ([aff755a](https://github.com/hangar-hub/hangar/commit/aff755a9667b3262d175ebfa082eafd4499bde8c))
* **upstream:** read an anonymous 401 from huggingface.co as not found ([33e5dc7](https://github.com/hangar-hub/hangar/commit/33e5dc7dfa535b33b09a5f942064b7fe7a0ab896))
* **upstream:** refuse redirects into the local network and reserved addresses ([65f13b7](https://github.com/hangar-hub/hangar/commit/65f13b7a02c59e9c54aad98c3edc2f50ad2d4ee7))
* **upstream:** stop another Namespace's quota starving a joined Pull ([7897293](https://github.com/hangar-hub/hangar/commit/7897293b217d084734ed6c4d1a9525bb2e54e23c))
* **upstream:** wait for the history of a Mirror that planning has just created ([7421fa9](https://github.com/hangar-hub/hangar/commit/7421fa919d9321f62865e7a25f655a186bec2aa5))
* **upstream:** wait for the history of a Mirror that planning has just created ([bf664e3](https://github.com/hangar-hub/hangar/commit/bf664e3fce2b2f29d3e886b19865d6f5f1b5807d))
* **viewer:** check the scan time limit only after rows are read ([0733bd8](https://github.com/hangar-hub/hangar/commit/0733bd870566f140c77c20a017937267a86764a3))
* **xet:** bound shard work, shard bodies and concurrent shard ingests ([266dab8](https://github.com/hangar-hub/hangar/commit/266dab86f57afa3d2de716386d8ce674793c3795))
* **xet:** bound the xorb index memory and file check memory of a shard ([9690a7c](https://github.com/hangar-hub/hangar/commit/9690a7c27397ab91e82731218eb93cf35c4c0b86))
* **xet:** charge xorb index loads against what a shard names ([c155bfe](https://github.com/hangar-hub/hangar/commit/c155bfe5cb6cfcaabfa564a7fd45e533a1bfc8b5))


### Performance

* **auth:** skip the linked OIDC providers when authenticating a request ([39d739b](https://github.com/hangar-hub/hangar/commit/39d739b60a8a516db3a504b892614e724bfa08be))
* **hfapi:** check read and the action with one permission check ([7857795](https://github.com/hangar-hub/hangar/commit/785779503a169bec7cec6829f1a207a2cf6f3123))
* **hfapi:** look a Repo up once per Hub read request ([ed2ef92](https://github.com/hangar-hub/hangar/commit/ed2ef92a4eb0953efb24b9dbe3effd4c2eed502b))
* **repo:** check small server-side Commits against the tree limits without git ([2188682](https://github.com/hangar-hub/hangar/commit/2188682f814ce7af78260f5f912254ff1cdfd9b7))
* **repo:** read a file's tree row before checking the tree cache ([8ae082e](https://github.com/hangar-hub/hangar/commit/8ae082e2e1c0ed8b26a27a0b41d302030c7b4bc9))
* **repo:** read trees for the push check with one git process ([bdd53d3](https://github.com/hangar-hub/hangar/commit/bdd53d369061f27d70e75a4902630ffb9ba375c9))
* **repo:** start a fixed number of git processes per request ([6332b50](https://github.com/hangar-hub/hangar/commit/6332b504bc78ebdfe868bd67d192242134b8a9db))
* **upstream:** fetch a Discover detail's card, dependencies and Viewer answers at once ([8dc2c71](https://github.com/hangar-hub/hangar/commit/8dc2c71f1ef687f3a335f42094400256368e6742))


### Documentation

* add a script that records the Home page demo ([99ac4ab](https://github.com/hangar-hub/hangar/commit/99ac4ab119e151bcb120c30b109a135d0d6da915))
* add load test results and the Phase 3 performance handoff ([14e859e](https://github.com/hangar-hub/hangar/commit/14e859ef63d083c91ff9d562788b14f221280cbf))
* add the 30-second demo to the Home page ([29baf84](https://github.com/hangar-hub/hangar/commit/29baf84502a3583903cc7fe06bd6d64f56c503bc))
* add the documentation site, repository files and release, docs and compatibility workflows ([07ec929](https://github.com/hangar-hub/hangar/commit/07ec9293063cec4a32890edf4f153dd9754a8f38))
* **blob:** say PresignGet forces the octet-stream Content-Type ([7436132](https://github.com/hangar-hub/hangar/commit/7436132dcaf2f6dd50bbb3bda8cd7f47e2424f12))
* **build:** add the phase 3 gaps handoff ([9ec1c60](https://github.com/hangar-hub/hangar/commit/9ec1c600efa5ba099b7db38fdafd3aed72bb8957))
* **build:** add the W6 handoff and requests, and the real-backend Upstream suite ([43ea957](https://github.com/hangar-hub/hangar/commit/43ea957e722f7d35069c65563e6a71ebcb7673ba))
* **build:** add the W7 handoff, requests and pickle scanning research ([3ce6a86](https://github.com/hangar-hub/hangar/commit/3ce6a86771fab61184cfc3b0f160ef5d623a87b2))
* **build:** add the W8 handoff and requests, and record the UI dependencies ([ccb75a5](https://github.com/hangar-hub/hangar/commit/ccb75a5cd9da728c000c64d7868b8b3174a40570))
* **build:** add the W9 handoff and requests, and record build tool dependencies ([85540b0](https://github.com/hangar-hub/hangar/commit/85540b08435df31dd321e9f6886d94811b61ac06))
* **build:** close the items left open after Phase 3 ([871d1bf](https://github.com/hangar-hub/hangar/commit/871d1bf0dfde0e81b3fbd240f2a34ffcd8f69a92))
* **build:** close W8 8 in the integration report with its commits and suite results ([a765ec0](https://github.com/hangar-hub/hangar/commit/a765ec09746a688410b91789aac1d24705199396))
* **build:** correct the W10 handoff on read errors and compat coverage ([ea82bca](https://github.com/hangar-hub/hangar/commit/ea82bca94752569ede5b4557f6ca8045823a86d2))
* **build:** correct the W9 handoff and record the verifier fixes ([c7c3930](https://github.com/hangar-hub/hangar/commit/c7c39301195bde8065f5fa9078d70c45a100c0cb))
* **build:** file the phase 3 decisions and open questions under their reviews ([d9c0eab](https://github.com/hangar-hub/hangar/commit/d9c0eab326cf1a714013fd5c756f362ec6ef5970))
* **build:** hand off the phase 3 authz review ([0c1b8a0](https://github.com/hangar-hub/hangar/commit/0c1b8a0fa2cf710f33007bd940337b642e367670))
* **build:** hand off the Phase 3 review of untrusted input ([d49578f](https://github.com/hangar-hub/hangar/commit/d49578f19ace499f420f50339419e0efc6c111b3))
* **build:** record James's Phase 3 decisions and the final check of every suite at a998d78 ([9815b88](https://github.com/hangar-hub/hangar/commit/9815b886684ff756758fe4308de76d1cf6b2a294))
* **build:** record Pull quota enforcement in the W6 handoff and requests ([c99b311](https://github.com/hangar-hub/hangar/commit/c99b3111e46549b3e9c5b977c32545e9d2aabf6c))
* **build:** record round 8 results, the Hub API MUSTs and the open import failure in the integration report ([c79363e](https://github.com/hangar-hub/hangar/commit/c79363e6833e3a34b6651f5596d5fce36afb360e))
* **build:** record sanitised Blob span errors and the AWS gap in the W1 handoff ([a119388](https://github.com/hangar-hub/hangar/commit/a1193884559ac918168440d9da11ed6ae7e725fe))
* **build:** record that the release is on hold until James says go ([2c15e82](https://github.com/hangar-hub/hangar/commit/2c15e8233a671d6d31bdb8a1b331f027c0f49b76))
* **build:** record the Compose and Helm smoke rerun and the download-path enumeration ([8d32835](https://github.com/hangar-hub/hangar/commit/8d32835d1a122f3d814f544612cb7c21b974de89))
* **build:** record the create_pr fix, upload_large_folder cells and Postgres runs in the W4 handoff ([e74034c](https://github.com/hangar-hub/hangar/commit/e74034c5cfebbbaa5d78810485d6ce1d83ff621c))
* **build:** record the final check of every suite at cd42d2b ([382cc88](https://github.com/hangar-hub/hangar/commit/382cc8836d3322941544af232d69f8657b277872))
* **build:** record the measured responsive result and the follow-up fixes in the W8 handoff ([948c7dc](https://github.com/hangar-hub/hangar/commit/948c7dcf29859d1e5f6fb2f3091fa6ff5b637ed7))
* **build:** record the merges after 5975bec and the request ID fix in the integration report ([4462828](https://github.com/hangar-hub/hangar/commit/44628285e06296016ad1cdb4892211061417812d))
* **build:** record the Namespace copy fix and the .gitkeep plugin in the W8 handoff ([fdbbc1a](https://github.com/hangar-hub/hangar/commit/fdbbc1a663eb847b8c0e106f7c5ce6392b1ec514))
* **build:** record the phase 3 docs walk-through and its handoff ([4ff146c](https://github.com/hangar-hub/hangar/commit/4ff146c64c065a8e653ed4363dbb84332981e847))
* **build:** record the Phase 3 input test runs ([858ee9c](https://github.com/hangar-hub/hangar/commit/858ee9ce7a9427c8e301212d30af34f051bbe4a7))
* **build:** record the pre-release check of every suite at 97d17c1 ([98fa3f2](https://github.com/hangar-hub/hangar/commit/98fa3f29f8373b01a0b8272d2fb812e128363768))
* **build:** record the requests phase 2 round 1 completed ([065fc0a](https://github.com/hangar-hub/hangar/commit/065fc0ad3624b5947c8c7b866bea45b9bfef65fc))
* **build:** record the rerun-script revert and the Postgres health check fix ([a25474e](https://github.com/hangar-hub/hangar/commit/a25474e8f4a1b5e7b8699705174d519b4478b1dc))
* **build:** record the round 9 chart fixes in the integration report ([f2e22bc](https://github.com/hangar-hub/hangar/commit/f2e22bc82ed52f9015a708e5ce63f763f3f74265))
* **build:** record the second W9 check and its fixes in the handoff ([2a1a631](https://github.com/hangar-hub/hangar/commit/2a1a6319ccca9ac621ac3c769b687154d66afdc4))
* **build:** record the shared-transfer quota fix and the real quota run in the W6 handoff ([cf7d08b](https://github.com/hangar-hub/hangar/commit/cf7d08baeb1bcb814dbb51a4d98700302d2f27ca))
* **build:** record the third W9 check and its fixes in the handoff ([3ed34a2](https://github.com/hangar-hub/hangar/commit/3ed34a206325d17bcd821029c493de5368490743))
* **build:** record the v1.0.0 release dry run ([ea2215e](https://github.com/hangar-hub/hangar/commit/ea2215eea2a078f5e370367753d19d05160d9935))
* **build:** record the W1 security fixes in the handoff and requests ([cac9d3e](https://github.com/hangar-hub/hangar/commit/cac9d3e601aeb2f0defb0147d5d919b237c7bae9))
* **build:** record the W11 index memory fixes and correct the follow-ups ([2da4b1b](https://github.com/hangar-hub/hangar/commit/2da4b1bd390ab9a3c245a5e5253e8f92bdf0c64c))
* **build:** record the W11 requests for wiring, auth and docs ([3587b4f](https://github.com/hangar-hub/hangar/commit/3587b4f55331113246a9a31b7c871d4c3cec1e6b))
* **build:** record the W11 security fixes and the xet.max_file_size request ([1ff5f52](https://github.com/hangar-hub/hangar/commit/1ff5f52dc752c40ac9063b8b12b39749beae4866))
* **build:** record the W6 compat run figures in the handoff ([6267b1d](https://github.com/hangar-hub/hangar/commit/6267b1db95e811de78956d4b1b122722720353a5))
* **build:** record W2 request 10 and rounds 2 and 3 in the integration report ([8176449](https://github.com/hangar-hub/hangar/commit/81764494b45e0bcb4553d8d05bea477c797342f6))
* **build:** record W2 request 10 in RESOLUTIONS.md ([3fc6cbe](https://github.com/hangar-hub/hangar/commit/3fc6cbe7b52953541afe780bf8601cfcbe50515c))
* **build:** record W6 phase 2 fix round 1 in the handoff ([1c2ab8a](https://github.com/hangar-hub/hangar/commit/1c2ab8a2c4abf5e89562468a185ed2f5dd6fc6c2))
* **build:** record W6 phase 2 fix round 2 in the handoff ([5a68862](https://github.com/hangar-hub/hangar/commit/5a6886225ee4ea1280ae69671eef02b7210d0fb5))
* **build:** record W6 phase 2 round 4 fixes in the handoff ([89ef2b4](https://github.com/hangar-hub/hangar/commit/89ef2b4145cbd2b4761fe8c629cedbf44283b27a))
* **build:** record W6 phase 2 round 5 in the handoff ([a17f874](https://github.com/hangar-hub/hangar/commit/a17f87447f6d4622fb683bd53f06c777c9d57efd))
* **build:** record W7 coverage in the handoff ([29aeaa3](https://github.com/hangar-hub/hangar/commit/29aeaa331521708ba3318c83993885e4be89a385))
* **build:** report the Phase 3 hardening and its round 4 suite results ([55ee4e2](https://github.com/hangar-hub/hangar/commit/55ee4e2673ae19260f7294a1e6a2d5c988036f4d))
* **build:** report the preview feedback round and its round 6 results ([c3eebeb](https://github.com/hangar-hub/hangar/commit/c3eebeb089510bf823e389f67a6433561078814d))
* **build:** report the verification round at 9f08db7 in the preview feedback section ([2954384](https://github.com/hangar-hub/hangar/commit/295438459cb345074414043f84843c451780fef2))
* **build:** update the W1 handoff after verification ([8fb372f](https://github.com/hangar-hub/hangar/commit/8fb372fa3507aa0237073862659c57283e534a9c))
* **build:** update the W2 handoff with the verification wait, fake changes and cross-branch runs ([468dbe4](https://github.com/hangar-hub/hangar/commit/468dbe4ce053f331ec240a81ed2c71bcc2b48e26))
* **build:** update the W8 handoff for upload resume, the LM Studio snippet and axe coverage, and ask W2 to reuse uploads in progress ([c00fbbe](https://github.com/hangar-hub/hangar/commit/c00fbbe1f131226328ae96f6a432842a89f0ae5b))
* **build:** write the integration report ([55c7064](https://github.com/hangar-hub/hangar/commit/55c70645ac3d047df3115f404ae38c68f9453f0e))
* **build:** write the W11 handoff and fix the token refresh assertion ([29b2e39](https://github.com/hangar-hub/hangar/commit/29b2e39527d6fde811ef981bd534153a245f0b9d))
* **compat:** describe the Upstream Pull suite; feat(api): add the Mirror summary for Repo responses ([7a499dd](https://github.com/hangar-hub/hangar/commit/7a499dd69c95aa28f88279ff37c32179f171f610))
* correct the demo GIF note and describe what the recording shows ([277065d](https://github.com/hangar-hub/hangar/commit/277065d336ccafe8cacc190e6403689ee92b1664))
* **datasets:** correct the W10 handoff on fuzzing, load and Postgres ([5cf8406](https://github.com/hangar-hub/hangar/commit/5cf84065a7ab8a8f0d8bf5033b1c0907d3b44eb3))
* **datasets:** record the delta and decimal fixes in the W10 handoff ([289ff65](https://github.com/hangar-hub/hangar/commit/289ff65f2d1951c885729c611d71d15976b26bc0))
* **datasets:** say a Mirror's Viewer needs the Upstream's Parquet export pulled ([13dd831](https://github.com/hangar-hub/hangar/commit/13dd831911d14521165e214b218b10cfd1e861d8))
* **datasets:** W10 handoff, requests and Hub differences ([ac3c768](https://github.com/hangar-hub/hangar/commit/ac3c7681b717e16d656485593783235b1cdf5dd3))
* fix what the get-started pages and guides got wrong ([5554cb5](https://github.com/hangar-hub/hangar/commit/5554cb53d032ec8af9930817171a1d8a49db9c7b))
* **helm:** read the generated Secret by the name the chart gives it ([1a0622e](https://github.com/hangar-hub/hangar/commit/1a0622e419d4d888e54debb7e92a24f360bfd599))
* **hfapi:** bring the W4 handoff in line with discard and likers ([08fa95d](https://github.com/hangar-hub/hangar/commit/08fa95d630103e343cf8b460e47a436e2de1fd8a))
* **operate:** document admin gc, reset-password, create-user and reindex ([bfa4af3](https://github.com/hangar-hub/hangar/commit/bfa4af3b561378b4d484c1c39ed071e8672efd89))
* publish the website at hangar.page ([0a1c993](https://github.com/hangar-hub/hangar/commit/0a1c9934811dc0e044b6e6d4f576df2cb7a01483))
* **readme:** set publicURL in the Helm install command ([54d944b](https://github.com/hangar-hub/hangar/commit/54d944b6f026a866c52f1cbae72db43348f77242))
* **readme:** show W8's screenshots of the Arsenal, a model card, the GGUF viewer and the Dataset Viewer ([203fde8](https://github.com/hangar-hub/hangar/commit/203fde89f7c5e1f7d1d2fe79188a8389886fce31))
* record phase 2 request resolutions and apply the doc changes they ask for ([5ec1bc2](https://github.com/hangar-hub/hangar/commit/5ec1bc2dcf626f81b02a16c453692b4b2e071bba))
* record self-service profile and password changes ([1cb7d6f](https://github.com/hangar-hub/hangar/commit/1cb7d6fe16f749a2414c4502a9358c68ad0b9860))
* record that self-set emails need a confirmation link ([ef94378](https://github.com/hangar-hub/hangar/commit/ef94378c637396e1940697e0b9992561617b9ca8))
* record the bellatrix upload fixes in the CLI help, guides and handoffs ([dbbe9fb](https://github.com/hangar-hub/hangar/commit/dbbe9fb298c7f18a3dc9ec3c3f2c9f6106242b34))
* record the input limits, network policy and symbolic link rules ([1b1d401](https://github.com/hangar-hub/hangar/commit/1b1d4014948927ebcd2dc74395a3b87d5e1a72d7))
* record the scanner and download gate review ([ad9cf7e](https://github.com/hangar-hub/hangar/commit/ad9cf7e0cf75343740763f6645dc144f07c92ac7))
* **reference:** regenerate the CLI reference with the new admin commands ([03953da](https://github.com/hangar-hub/hangar/commit/03953da616d6e8dd358d1f5817506ff75b1e1b8d))
* regenerate the screenshots ([aad25d7](https://github.com/hangar-hub/hangar/commit/aad25d76453db338144b156f765fb0297494f5ae))
* **research:** record the AWS S3 results ([1c4e256](https://github.com/hangar-hub/hangar/commit/1c4e256a26da1b679a366696a129b453d0627e94))
* **research:** use British spellings in the Xet upload research ([13fadae](https://github.com/hangar-hub/hangar/commit/13fadae3a7491869e766a038b3c0ad938741f799))
* **research:** use British spellings in the Xet upload research ([da93189](https://github.com/hangar-hub/hangar/commit/da931894cf484896b8a64719865967c3831657d7))
* run the chart with Argo CD, use MinIO's own region and stream large files through nginx ([d91688f](https://github.com/hangar-hub/hangar/commit/d91688f5a9a6e928d0166163e1085f5d3870ea41))
* say only, not just, where a quant or Split is chosen ([6bfb7e9](https://github.com/hangar-hub/hangar/commit/6bfb7e9924bc11cf12a63422673894ca8b9046c9))
* **security:** add the threat model ([132da07](https://github.com/hangar-hub/hangar/commit/132da070640d754eb74048a40770605cc151875c))
* **security:** link the threat model to spec 04 on GitHub, which the site does not publish ([9ca750d](https://github.com/hangar-hub/hangar/commit/9ca750dafe2d1c4b9fa82090c67a3462586506c0))
* serve hangar.page from Cloudflare Pages ([41740e9](https://github.com/hangar-hub/hangar/commit/41740e909c48483671cddc5eaa5f41a50d91b7d4))
* **spec:** record the bound on concurrent password hashing ([98a6457](https://github.com/hangar-hub/hangar/commit/98a6457a7042712014f673573583d8ca9eafe94e))


### Dependencies

* update github.com/getkin/kin-openapi to v0.151.0 ([c4668be](https://github.com/hangar-hub/hangar/commit/c4668bef24a47f4c9f6879c926ac2fcc5db770b7))
