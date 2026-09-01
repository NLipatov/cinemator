# Changelog

## [0.6.1](https://github.com/NLipatov/cinemator/compare/0.6.0...0.6.1) (2026-09-01)


### Bug Fixes

* **streaming:** continue hls preparation without viewers ([#28](https://github.com/NLipatov/cinemator/issues/28)) ([a76ce72](https://github.com/NLipatov/cinemator/commit/a76ce72b3eea6a162b31cda0a0722029f6f8f9e8))

## [0.6.0](https://github.com/NLipatov/cinemator/compare/0.5.1...0.6.0) (2026-09-01)


### Features

* **torrent:** persist completed hls streams ([#26](https://github.com/NLipatov/cinemator/issues/26)) ([99460e8](https://github.com/NLipatov/cinemator/commit/99460e88f0851d0c9d24c221f40db99c8961b14e))

## [0.5.1](https://github.com/NLipatov/cinemator/compare/0.5.0...0.5.1) (2026-08-31)


### Bug Fixes

* **torrent:** release deleted file storage ([#24](https://github.com/NLipatov/cinemator/issues/24)) ([cc6e4ee](https://github.com/NLipatov/cinemator/commit/cc6e4ee317be5eed8cf792804b4b02f868144366))

## [0.5.0](https://github.com/NLipatov/cinemator/compare/0.4.0...0.5.0) (2026-08-24)


### Features

* **auth:** add QR sign-in approval ([439d58a](https://github.com/NLipatov/cinemator/commit/439d58adaf0f7aa1918c168ce3898098ca1ad3fa))
* **auth:** add QR sign-in approval ([b85717b](https://github.com/NLipatov/cinemator/commit/b85717b51f3cc9c2937d9c38f07e329f1690f163))


### Bug Fixes

* **auth:** harden QR sign-in requests ([ad413a6](https://github.com/NLipatov/cinemator/commit/ad413a684fb5ffa831ec307a030c1e091401d8da))
* **auth:** require public origin for QR sign-in ([f4b8955](https://github.com/NLipatov/cinemator/commit/f4b895506f401df2ebd74ad7cd4c2455d86fd509))

## [0.4.0](https://github.com/NLipatov/cinemator/compare/0.3.0...0.4.0) (2026-08-21)


### Features

* **web:** display build version ([b70f07f](https://github.com/NLipatov/cinemator/commit/b70f07f9baeb3a337c0bc302a9fb575a4699b581))
* **web:** simplify playback interface ([b6056cb](https://github.com/NLipatov/cinemator/commit/b6056cb595a8ac2c0799877df5ac2ba05d62740c))


### Bug Fixes

* **app:** harden streaming and refresh playback UI ([7765a74](https://github.com/NLipatov/cinemator/commit/7765a7446d7aeebb38963858c7b2a02715536639))
* **downloads:** wait for active streams before cleanup ([f71f6da](https://github.com/NLipatov/cinemator/commit/f71f6da3b1cb5fdef04eec768561ffdc4ae4de12))
* **player:** reset native hls source on switch ([5a27640](https://github.com/NLipatov/cinemator/commit/5a27640a762681301dcfddb3047528361b8b0f2f))
* resolve cleanup and accessibility regressions ([9118852](https://github.com/NLipatov/cinemator/commit/91188520a51f5a0e5cf932cdffd461f8b45742f1))
* **streams:** make teardown context-aware ([87a477e](https://github.com/NLipatov/cinemator/commit/87a477ee0c0262e828765e5746edf92ebf2140b9))
* **streams:** serialize cleanup and replacement ([bb3b9db](https://github.com/NLipatov/cinemator/commit/bb3b9db096ae43fb39e255b5fd0e45e69258422a))
* **torrent:** harden stream cleanup serialization ([fd10962](https://github.com/NLipatov/cinemator/commit/fd1096289cafbd5fbf68acfdd29516e124061e19))
* **torrent:** reject unsupported v2-only magnets ([8e1ecac](https://github.com/NLipatov/cinemator/commit/8e1ecacde53c191d76106e998276c321cf104b7a))
* **torrent:** serialize add and deletion ([bb3324f](https://github.com/NLipatov/cinemator/commit/bb3324f637e8d3d4cea5a6be628185ec4206dee3))
* **web:** address review feedback ([070e9d0](https://github.com/NLipatov/cinemator/commit/070e9d000fc66dd1f9621ce3dad325e0f17f8494))

## [0.3.0](https://github.com/NLipatov/cinemator/compare/0.2.1...0.3.0) (2026-07-16)


### Features

* add Caddy deployment and optional app authentication ([d3f7271](https://github.com/NLipatov/cinemator/commit/d3f72710563598cf747fa0176e51abe44b27abd8))


### Bug Fixes

* **ci:** restore Caddy release metadata ([ea023d7](https://github.com/NLipatov/cinemator/commit/ea023d7f671c2fa458e14ed8ffa09eacd8b668fc))

## [0.2.1](https://github.com/NLipatov/cinemator/compare/0.2.0...0.2.1) (2026-07-13)


### Bug Fixes

* clean up abandoned stream preparations ([21b7216](https://github.com/NLipatov/cinemator/commit/21b721634b73c8b67e8dabdc90945c0c35fb1f49))
* correct HLS playback and subtitle synchronization ([a4299ef](https://github.com/NLipatov/cinemator/commit/a4299efc1ee91c5643e38c0047a6d08702e40a0e))

## [0.2.0](https://github.com/NLipatov/cinemator/compare/0.1.20...0.2.0) (2026-07-09)


### Features

* add download lifecycle management ([1ba5cb8](https://github.com/NLipatov/cinemator/commit/1ba5cb854b6e503ee820e67f926cf7f76d042430))


### Bug Fixes

* address download manager review feedback ([d8c8b11](https://github.com/NLipatov/cinemator/commit/d8c8b11b0b82ef34ff0d75fe7c688109bf06cf37))
* fail empty subtitle tracks explicitly ([61d89d9](https://github.com/NLipatov/cinemator/commit/61d89d950d18637987c7077ee1aa3e0e64e2221a))
* improve download playback lifecycle ([752bf1d](https://github.com/NLipatov/cinemator/commit/752bf1d3c0e19d4077c9e32f2d9b273c845c87db))
* normalize subtitle playlist timing ([bb82211](https://github.com/NLipatov/cinemator/commit/bb82211bf4756b0d5d921950410c469726185e5c))

## [0.1.20](https://github.com/NLipatov/cinemator/compare/0.1.19...0.1.20) (2026-07-06)


### Bug Fixes

* address streaming review feedback ([dbb810f](https://github.com/NLipatov/cinemator/commit/dbb810fea5f4f9d31212d8f35d0bd42def456725))
* guard HLS conversion resumes ([b5ac1cd](https://github.com/NLipatov/cinemator/commit/b5ac1cdede9bfd37e3ef7569367506733fc07d2a))
* speed up media track probing ([5446385](https://github.com/NLipatov/cinemator/commit/54463858682c3efca94037476726e7b712887006))
