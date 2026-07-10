# russian_trusted_root_ca.pem

Корневой сертификат «Russian Trusted Root CA» НУЦ Минцифры РФ (публичный,
notAfter: 2032-02-27). Скачан с официального источника Госуслуг
(https://www.gosuslugi.ru/crt → https://gu-st.ru/content/lending/russian_trusted_root_ca_pem.crt).

Нужен для TLS-соединений с хостами GigaChat API
(`ngw.devices.sberbank.ru`, `gigachat.devices.sberbank.ru`) — они подписаны
этим корнем, которого нет в системных сторах за пределами РФ. Вшивается в
бинарь через `go:embed` и добавляется к системному пулу корней только в
HTTP-клиенте этого пакета.
