# 🚀 ShareLoom - Presigned URL Generator Service (AWS Lambda + Go)

Este microservicio Serverless forma parte del ecosistema **ShareLoom** (Nube Temporal Enterprise). Su función principal es registrar la metadata de caducidad en Amazon DynamoDB y generar una **S3 Pre-signed URL** segura para permitir la subida directa de archivos cifrados (*Zero-Knowledge*) desde el cliente sin recargar el backend.

---

## 🏗️ Arquitectura y Tecnologías

* **Lenguaje:** Go 1.22+
* **Runtime:** AWS Lambda (`provided.al2023`)
* **Integraciones AWS SDK v2:**
  * **Amazon S3:** Generación de Pre-signed URLs para subida directa (método `PUT`, validez de 15 min).
  * **Amazon DynamoDB:** Registro de metadatos del archivo con bandera **TTL** (`expireAt` a 24 horas).
  * **AWS CloudWatch:** Emisión de métricas y registros de auditoría.

---

## 🔑 Variables de Entorno

El servicio requiere la siguiente variable de entorno configurada en AWS Lambda:

| Variable | Descripción | Ejemplo |
| :--- | :--- | :--- |
| `BUCKET_NAME` | Nombre del bucket S3 privado donde se alojan los archivos cifrados. | `shareloom-vault-jp2026` |

---

## 🛠️ Compilación Local

Dado que la función se despliega en el runtime personalizado **Amazon Linux 2023 (`provided.al2023`)**, el ejecutable debe nombrarse obligatoriamente `bootstrap` y compilarse para arquitectura `linux/amd64`.

### Windows (PowerShell)
```powershell
$env:GOOS="linux"; $env:GOARCH="amd64"; $env:CGO_ENABLED="0"; go build -o bootstrap main.go
Compress-Archive -Path bootstrap -DestinationPath function.zip -Force
```

### Linux / macOS (Bash)
```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bootstrap main.go
zip function.zip bootstrap
```

> **Nota:** Los archivos `bootstrap` y `function.zip` están excluidos del control de versiones mediante `.gitignore`.

---

## 📡 Contrato de la API

### Endpoint: `POST /upload`

#### Respuesta Exitosa (`200 OK`)
```json
{
  "uploadUrl": "https://shareloom-vault.s3.amazonaws.com/f47ac10b-58cc-4372-a567-0e02b2c3d479?X-Amz-Algorithm=...",
  "fileId": "f47ac10b-58cc-4372-a567-0e02b2c3d479"
}
```

#### Respuesta de Error (`500 Internal Server Error`)
```json
{
  "error": "Failed to save metadata: ..."
}
```

---

## 🔐 Seguridad y Permisos (IAM)

El rol de ejecución de esta función requiere permisos de menor privilegio (*Least Privilege*) sobre los siguientes servicios:

* `s3:PutObject` sobre el bucket configurado en `BUCKET_NAME`.
* `dynamodb:PutItem` sobre la tabla `ShareLoomMetadata`.
* `logs:CreateLogGroup`, `logs:CreateLogStream`, `logs:PutLogEvents` para CloudWatch.
