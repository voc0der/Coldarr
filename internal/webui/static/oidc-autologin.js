(function () {
  "use strict";

  var login = document.querySelector("#oidc-login[data-auto-login]");
  if (login) {
    // This script runs from the committed same-origin login document. Using
    // replace keeps that transition page out of the browser's back history.
    window.location.replace(login.href);
  }
})();
