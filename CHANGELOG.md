# Changelog

## [0.1.0-alpha.1](https://github.com/hop-top/heo/compare/v0.1.0-alpha.0...v0.1.0-alpha.1) (2026-11-20)


### Features

* **cli:** --message targeting for state verbs, tag and unsubscribe ([#13](https://github.com/hop-top/heo/issues/13)) ([6aee826](https://github.com/hop-top/heo/commit/6aee826681c108cb8943aa62f5e1f0c87db9a71e))
* **cli:** message forward; message list by Message-ID, References, provider ID ([#14](https://github.com/hop-top/heo/issues/14)) ([3235412](https://github.com/hop-top/heo/commit/323541248c9726e7b4bf967c478feee5c5dec245))
* **conversation:** last activity, --sort last-activity, --updated-since on list ([36eb84b](https://github.com/hop-top/heo/commit/36eb84bb5a465af9c39107b3a95632b8f47e453c))
* **desk:** attention list merges waiting conversations and held operations ([41b7028](https://github.com/hop-top/heo/commit/41b7028d903aec0227359f7207d89ca7dc3906e7))
* **desk:** read-marker and snooze-wake events, snooze-ended list filter ([#11](https://github.com/hop-top/heo/issues/11)) ([ad2b178](https://github.com/hop-top/heo/commit/ad2b178c538ce895d91086295e540244033c8558))
* **events:** inbox, verb and target on held and decided operation events ([8df3a38](https://github.com/hop-top/heo/commit/8df3a38eab4b70abf6de81bda18dc9f270b6781b))


### Bug Fixes

* **channels:** check needs one receive way, not both ([283b36a](https://github.com/hop-top/heo/commit/283b36a0aac3d4fcbe7019e61d095b743549e232))
* **daemon,e2e:** canceled receivers start not ready; unique e2e free ports ([425e3fd](https://github.com/hop-top/heo/commit/425e3fdfb3ac5b4f435d648d150cb90bbebdf59a))
* **hooks:** socket owner check builds on windows ([dafc4a8](https://github.com/hop-top/heo/commit/dafc4a8c8320616ec40d945934650f37181c2cec))


### Performance

* **cmd:** reflect each output schema once per process ([2ccef7b](https://github.com/hop-top/heo/commit/2ccef7b7cb92c891be86427bf037fb722fd5c179))

## 0.1.0-alpha.0 (2026-11-17)


### Features

* initial public release ([f53c203](https://github.com/hop-top/heo/commit/f53c203a45b3c70e3c3c869c9c0d3473608bce40))
