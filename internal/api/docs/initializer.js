window.addEventListener('load', function () {
  SwaggerUIBundle({
    url: '/openapi.json',
    dom_id: '#swagger-ui',
    deepLinking: true,
    validatorUrl: null,
    persistAuthorization: false,
    queryConfigEnabled: false,
    defaultModelsExpandDepth: -1,
    supportedSubmitMethods: ['get', 'post', 'patch', 'delete'],
    requestInterceptor: function (request) {
      if (new URL(request.url, window.location.href).origin !== window.location.origin) {
        throw new Error('O Swagger só pode chamar esta API.');
      }
      return request;
    }
  });
});
