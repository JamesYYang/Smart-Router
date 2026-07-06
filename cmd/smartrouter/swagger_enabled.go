//go:build swagger

package main

import swaggerdocs "smartrouter/cmd/smartrouter/docs"

func configureSwaggerDocs(basePath string) {
	swaggerdocs.SwaggerInfo.BasePath = basePath
}
