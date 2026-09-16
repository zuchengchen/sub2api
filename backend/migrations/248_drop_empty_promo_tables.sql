-- Drop leftover promo-code tables only when they have no business rows.
-- Historical 033_add_promo_codes.sql created these; HTTP routes are already gone.
DO $$
BEGIN
  IF to_regclass('public.promo_codes') IS NULL AND to_regclass('public.promo_code_usages') IS NULL THEN
    RETURN;
  END IF;

  IF to_regclass('public.promo_codes') IS NOT NULL THEN
    IF EXISTS (SELECT 1 FROM public.promo_codes LIMIT 1) THEN
      RAISE EXCEPTION 'promo_codes has business rows; refuse silent DROP';
    END IF;
  END IF;

  IF to_regclass('public.promo_code_usages') IS NOT NULL THEN
    IF EXISTS (SELECT 1 FROM public.promo_code_usages LIMIT 1) THEN
      RAISE EXCEPTION 'promo_code_usages has business rows; refuse silent DROP';
    END IF;
  END IF;

  DROP TABLE IF EXISTS public.promo_code_usages;
  DROP TABLE IF EXISTS public.promo_codes;
END $$;
