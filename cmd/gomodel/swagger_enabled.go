//go:build swagger

package main

import swaggerdocs "smartrouter/cmd/gomodel/docs"

func configureSwaggerDocs(basePath string) {
	swaggerdocs.SwaggerInfo.BasePath = basePath
}
