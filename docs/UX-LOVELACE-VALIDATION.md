# Validation du raffinement Lovelace

Validation terminée le 1er novembre 2026. Le résultat suit [le plan UX](UX-LOVELACE.md) : composants compacts, parcours accueil → pièce → équipement, présentations avancées réutilisables dans Slint Kit Pro et composition fournie par le serveur.

Les contrôles Go ciblés, Rust, génération transport, dépendances, fmt et Clippy passent. Pro passe 127 tests UI, 27 runtime et 4 build ; 20 captures natives finales sont revues. Le client normal est construit pour Mac et Android arm64.

Sur la Fire KFTUWI physique, 14 captures sont revues en paysage, portrait et sombre avec texte à 200 %. Un glissement réel de dimmer produit zéro intention pendant le maintien puis exactement une intention au relâchement, à 64 %, vers la liaison canonique attendue. Android Back remonte du thermostat détaillé à la pièce puis à l'accueil sans écriture supplémentaire. Les observations restent distinctes des intentions en attente. UID 10142, données de l'app et réglages d'origine sont conservés ; les préférences d'affichage et quatre paramètres Android sont restaurés.

La qualification utilise le client normal, les services réels de configuration/authentification/dashboard/listes et des pilotes terminaux synthétiques isolés. Aucun ordre n'est envoyé aux équipements HA réels. Le serveur de production ne propose pas encore MediaService et CalendarService : les composants média Pro sont qualifiés séparément, sans inventer de contenu dans le dashboard. Les alias hors manifeste sont inertes et la liste privée sans correspondance de compte est exclue. Cette revue ne revendique ni intégration matérielle complète ni mesure de latence.

Preuves locales conservées dans `/Users/damien/Projects/Homeward-artifacts/ux-lovelace-reference` :

- `DELIVERY.md`, `integration-gates.json`, `configuration-selector-audit.json` ;
- `desktop-artifact.json`, `android-artifact.json` ;
- `pro/source-qualified.json`, `pro/native-qualified/review.json` ;
- `ux-physical-final-review.md`, `ux-physical-final-review.json` ;
- `live-review-stable/tablet-qualification.json`, captures et reçus publics associés ;
- `live-review-stable/tls-validity.json` : certificat couvrant les douze heures de revue, avec expiration vérifiée par test X.509.

La session locale reste ouverte pour revue jusqu'au 2 novembre à 04 h 51, heure de Toronto, avec l'endpoint synthétique conservé intentionnellement sur Mac et tablette. Les fichiers privés de session ne font pas partie des preuves publiques. La suite complète des migrations historiques serveur n'est pas revendiquée ; aucun schéma SQL n'est changé par ce raffinement.
